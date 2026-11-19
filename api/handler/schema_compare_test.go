package handler

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dbgold/diff"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSchemaCompareEmptyTableSelectionUsesWholeSchema(t *testing.T) {
	req := schemaCompareRequest{
		Source: schemaCompareEndpoint{ConnectionID: 1, Namespace: "source"},
		Target: schemaCompareEndpoint{ConnectionID: 2, Namespace: "target"},
		Tables: []string{},
	}
	require.NoError(t, validateSchemaCompareRequest(&req))
	require.Nil(t, req.Tables)
}

func TestSchemaComparePreviewRespectsOwnerExpiryAndSingleUse(t *testing.T) {
	cache := schemaComparePlanCache{plans: map[string]*schemaComparePlan{}}
	now := time.Now()
	valid := &schemaComparePlan{ID: "test", owner: 1, ExpiresAt: now.Add(time.Minute)}
	cache.save(valid, now)
	require.Nil(t, cache.get(valid.ID, 2, now, true))
	require.Same(t, valid, cache.get(valid.ID, 1, now, false))
	require.Same(t, valid, cache.get(valid.ID, 1, now, true))
	require.Nil(t, cache.get(valid.ID, 1, now, true))

	cache.save(valid, now)
	require.Nil(t, cache.get(valid.ID, 1, valid.ExpiresAt, false))
}

func TestSchemaComparePreviewCanOnlyBeClaimedOnceConcurrently(t *testing.T) {
	cache := schemaComparePlanCache{plans: map[string]*schemaComparePlan{}}
	now := time.Now()
	plan := &schemaComparePlan{ID: "shared", owner: 7, ExpiresAt: now.Add(time.Minute)}
	cache.save(plan, now)

	var workers sync.WaitGroup
	claims := make(chan *schemaComparePlan, 8)
	for range cap(claims) {
		workers.Go(func() { claims <- cache.get(plan.ID, plan.owner, now, true) })
	}
	workers.Wait()
	close(claims)
	successful := 0
	for claim := range claims {
		if claim == plan {
			successful++
		}
	}
	require.Equal(t, 1, successful)
}

func TestSchemaApplyRejectsClientSQL(t *testing.T) {
	router := gin.New()
	router.POST("/apply", ApplyTableSchemaChanges)
	req := httptest.NewRequest(http.MethodPost, "/apply",
		strings.NewReader(`{"plan_id":"test","change_ids":["id"],"sql":["ALTER TABLE example ADD x integer"]}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestSchemaApplySelectionRejectsMissingDependencyManualAndTamperedID(t *testing.T) {
	plan := &schemaComparePlan{Changes: []diff.Change{
		{ID: "create", Status: diff.Applicable, SQL: []string{"CREATE"}},
		{ID: "comment", Status: diff.Applicable, SQL: []string{"COMMENT"}, DependsOn: []string{"create"}},
		{ID: "manual", Status: diff.Manual},
	}}
	for _, invalid := range [][]string{{"comment"}, {"manual"}, {"create", "made-up"}, {"create", "create"}, {}} {
		_, err := selectSchemaChanges(plan, invalid)
		require.Error(t, err, "selection %v", invalid)
	}
	selected, err := selectSchemaChanges(plan, []string{"comment", "create"})
	require.NoError(t, err)
	require.Equal(t, plan.Changes[:2], selected)
}

func TestSchemaApplyGroupsNewTableAndIndexesBeforeForeignKeys(t *testing.T) {
	groups := groupSchemaChanges([]diff.Change{
		{ID: "fk", Table: "A", Order: 50, DependsOn: []string{"A", "B"}},
		{ID: "idx", Table: "A", Order: 30},
		{ID: "B", Table: "B", Order: 10},
		{ID: "A", Table: "A", Order: 10},
	})
	require.Len(t, groups, 3)
	require.Equal(t, "A", groups[0].changes[0].ID)
	require.Equal(t, "idx", groups[0].changes[1].ID)
	require.Equal(t, "B", groups[1].changes[0].ID)
	require.Equal(t, "fk", groups[2].changes[0].ID)
}

func TestSchemaApplyDDLFailureIsolatesOtherTables(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("DDL A").WillReturnError(errors.New("invalid column"))
	mock.ExpectExec("DDL B").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()

	results := executeSchemaChanges(context.Background(), db, diff.Catalog{Product: "dameng"}, []diff.Change{
		{ID: "a", Table: "A", Order: 10, SQL: []string{"DDL A"}},
		{ID: "a2", Table: "A", Order: 20, SQL: []string{"DDL A2"}},
		{ID: "b", Table: "B", Order: 10, SQL: []string{"DDL B"}},
		{ID: "fk", Table: "B", Order: 50, DependsOn: []string{"a", "b"}, SQL: []string{"DDL FK"}},
	}, nil)
	require.Len(t, results, 4)
	require.Equal(t, schemaFailed, results[0].Status)
	require.Equal(t, schemaSkipped, results[1].Status)
	require.Equal(t, schemaApplied, results[2].Status)
	require.Equal(t, schemaSkipped, results[3].Status)
	require.NoError(t, db.Close())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchemaApplyUnknownDDLStopsSubsequentChanges(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("DDL A").WillReturnError(driver.ErrBadConn)
	mock.ExpectClose()

	results := executeSchemaChanges(context.Background(), db, diff.Catalog{Product: "dameng"}, []diff.Change{
		{ID: "a", Table: "A", SQL: []string{"DDL A"}},
		{ID: "b", Table: "B", SQL: []string{"DDL B"}},
	}, nil)
	require.Len(t, results, 2)
	require.Equal(t, schemaUnknown, results[0].Status)
	require.Equal(t, schemaSkipped, results[1].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchemaApplyGuardsMySQLCommentWithWriteLock(t *testing.T) {
	const original = "CREATE TABLE `Orders` (`Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'old', PRIMARY KEY (`Id`)) ENGINE=InnoDB"
	const update = "ALTER TABLE `app`.`Orders` MODIFY COLUMN `Id` bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'new'"
	for _, tc := range []struct {
		name, previousComment, status string
	}{
		{"same column", "old", schemaApplied},
		{"comment modified after catalog reread", "original", schemaFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectExec("SET SESSION lock_wait_timeout=5").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("LOCK TABLES `app`.`Orders` WRITE").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("SET SESSION sql_mode='', sql_quote_show_create=1").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SHOW CREATE TABLE `app`.`Orders`").
				WillReturnRows(sqlmock.NewRows([]string{"Table", "Create Table"}).AddRow("Orders", original))
			if tc.status == schemaApplied {
				mock.ExpectExec(schemaMySQLDDLCommentMode).WithArgs("NO_ZERO_DATE").WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(update).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec("UNLOCK TABLES").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectClose()
			status, err := executeSchemaStatementChange(context.Background(), db, diff.Catalog{Product: "mysql", Namespace: "app", Mode: "NO_ZERO_DATE"},
				diff.Change{Table: "Orders", Object: "Id", Kind: "column_comment", Expected: "new", Target: tc.previousComment, SQL: []string{update}})
			require.Equal(t, tc.status, status)
			if tc.status == schemaApplied {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "目标列定义已变化")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
