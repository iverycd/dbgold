package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// PG can silently skip IF NOT EXISTS for a relation on an unrelated table.
// For converted or disambiguated MySQL keys, create strictly and only reuse a
// duplicate after verifying the physical definition on the intended table.
func createPGIndex(ctx context.Context, db *sql.DB, d dialect.Dialect, schema string, idx source.IndexInfo) error {
	spatial := strings.EqualFold(idx.Method, "gist")
	if spatial {
		caps, err := InspectPGSpatial(ctx, db)
		if err != nil {
			return err
		}
		if caps.GISTError != "" {
			return fmt.Errorf("目标空间索引能力不足: %s", caps.GISTError)
		}
		if idx.PGSpatialSchema != caps.GISTSchema || idx.PGSpatialOpClass != caps.GISTOpClass {
			return fmt.Errorf("目标空间操作符类与索引计划不一致，请重新生成迁移计划")
		}
	}
	query := dialect.JoinSQL(d.IndexStatements(schema, idx))
	if !spatial && len(idx.MySQLPrefixLengths) == 0 && idx.TargetIndexName == "" {
		_, err := execObserved(ctx, db, query)
		return err
	}
	var verificationErr error
	verifyDuplicate := func(createErr error) bool {
		if compatibilitySQLState(createErr) != "42P07" {
			return false
		}
		if schema == "" {
			if verificationErr = db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); verificationErr != nil {
				return false
			}
		}
		result, err := inspectRepairIndex(ctx, repairTarget{db: db, schema: schema, family: "postgres"}, repair.Object{
			Schema: schema, Table: idx.TableName, TargetName: dialect.IndexName(d, idx), Index: &idx,
		})
		verificationErr = err
		if err == nil && result.Status != "succeeded" {
			verificationErr = fmt.Errorf("核验同名索引失败: %s", result.Message)
		}
		return verificationErr == nil
	}
	_, err := execObservedAllow(ctx, db, query, verifyDuplicate)
	return errors.Join(err, verificationErr)
}
