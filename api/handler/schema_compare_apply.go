package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/diff"
	"dbgold/middleware"
	"dbgold/store"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

type schemaApplyRequest struct {
	PlanID    string   `json:"plan_id"`
	ChangeIDs []string `json:"change_ids"`
}

type schemaApplyResult struct {
	ID     string `json:"id"`
	Table  string `json:"table"`
	Object string `json:"object,omitempty"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

const (
	schemaApplied = "applied"
	schemaFailed  = "failed"
	schemaSkipped = "skipped"
	schemaUnknown = "unknown"
)

func schemaApplyConflict(c *gin.Context, message string) {
	c.JSON(http.StatusConflict, gin.H{"error": message})
}

func selectSchemaChanges(plan *schemaComparePlan, ids []string) ([]diff.Change, error) {
	if len(ids) == 0 {
		return nil, errors.New("请选择需要应用的差异")
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		if selected[id] {
			return nil, errors.New("应用差异包含重复项")
		}
		selected[id] = true
	}
	var changes []diff.Change
	for _, change := range plan.Changes {
		if !selected[change.ID] {
			continue
		}
		if change.Status != diff.Applicable || len(change.SQL) == 0 {
			return nil, fmt.Errorf("差异 %s 无法直接应用", change.ID)
		}
		for _, dependency := range change.DependsOn {
			if !selected[dependency] {
				return nil, errors.New("请同时选择所需的依赖差异")
			}
		}
		changes = append(changes, change)
	}
	if len(changes) != len(ids) {
		return nil, errors.New("选择包含不在当前预览中的差异")
	}
	return changes, nil
}

func lockSchemaApplyTarget(conn *store.Connection, plan *schemaComparePlan) (func(), error) {
	// Existing migration callers key MySQL by the saved default database as
	// well as the selected namespace. Also lock the effective database scope,
	// so plans using different connection records serialize with each other.
	scopes := []string{plan.targetScope, migrationScope(conn, plan.targetSchema)}
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	owner := uuid.NewString()
	var acquired []string
	release := func() {
		for _, scope := range acquired {
			if err := store.ReleaseMigrationScope(scope, owner); err != nil {
				slog.Error("release schema change scope", "plan_id", plan.ID, "err", err)
			}
		}
	}
	for _, scope := range scopes {
		if err := store.AcquireMigrationScope(scope, owner); err != nil {
			release()
			return nil, err
		}
		acquired = append(acquired, scope)
	}
	return release, nil
}

func ApplyTableSchemaChanges(c *gin.Context) {
	var req schemaApplyRequest
	if !decodeSchemaCompare(c, &req) {
		return
	}
	owner := middleware.GetCurrentUserID(c)
	plan := schemaComparePlans.get(req.PlanID, owner, time.Now(), false)
	if plan == nil {
		schemaApplyConflict(c, "结构比对预览已失效，请重新比对")
		return
	}
	changes, err := selectSchemaChanges(plan, req.ChangeIDs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	srcConn, dstConn, ok := ownedSchemaCompareConnections(c, plan.request)
	if !ok {
		return
	}
	if plan.sourceConfig != schemaConnectionDigest(srcConn) || plan.targetConfig != schemaConnectionDigest(dstConn) {
		schemaApplyConflict(c, "源库或目标库连接配置已变化，请重新比对")
		return
	}
	release, err := lockSchemaApplyTarget(dstConn, plan)
	if errors.Is(err, store.ErrRepairConflict) {
		schemaApplyConflict(c, "目标库正在执行其他迁移或结构变更，请稍后重试")
		return
	}
	if err != nil {
		schemaCompareError(c, err, dstConn)
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(c.Request.Context(), schemaApplyTimeout)
	defer cancel()
	src, err := openSchemaCompareDB(srcConn, plan.request.Source.Namespace)
	if err != nil {
		schemaCompareError(c, err, srcConn)
		return
	}
	defer src.Close()
	dst, err := openSchemaCompareDB(dstConn, plan.request.Target.Namespace)
	if err != nil {
		schemaCompareError(c, err, dstConn)
		return
	}
	defer dst.Close()
	sourceCatalog, targetCatalog, err := readSchemaCompareCatalogs(ctx, src, dst, srcConn, dstConn, plan.request)
	if err != nil {
		schemaCompareError(c, err, srcConn, dstConn)
		return
	}
	if plan.sourceDigest != repair.Hash(sourceCatalog) || plan.targetDigest != repair.Hash(targetCatalog) {
		schemaApplyConflict(c, "源库或目标库结构已变化，请重新比对")
		return
	}
	// Resolve both connections again before the single-use preview is claimed.
	// A saved connection may have changed while the catalogs were loading.
	currentSource, currentTarget, ok := ownedSchemaCompareConnections(c, plan.request)
	if !ok {
		return
	}
	if !store.SameConnectionExecution(srcConn, currentSource) || !store.SameConnectionExecution(dstConn, currentTarget) {
		schemaApplyConflict(c, "源库或目标库连接配置已变化，请重新比对")
		return
	}
	if schemaComparePlans.get(req.PlanID, owner, time.Now(), true) == nil {
		schemaApplyConflict(c, "结构比对预览已失效，请重新比对")
		return
	}
	slog.Info("schema changes started", "plan_id", plan.ID, "owner_id", owner,
		"source_connection_id", srcConn.ID, "target_connection_id", dstConn.ID,
		"source_namespace", sourceCatalog.Namespace, "target_namespace", targetCatalog.Namespace, "change_ids", req.ChangeIDs)
	results := executeSchemaChanges(ctx, dst, targetCatalog, changes, func(result schemaApplyResult) {
		logSchemaCompareResult(plan, result)
	})
	for i := range results {
		if results[i].Error != "" {
			results[i].Error = schemaCompareErrorText(errors.New(results[i].Error), dstConn)
		}
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

type schemaApplyGroup struct {
	table   string
	late    bool
	changes []diff.Change
}

func groupSchemaChanges(changes []diff.Change) []schemaApplyGroup {
	ordered := slices.Clone(changes)
	slices.SortStableFunc(ordered, func(a, b diff.Change) int {
		if (a.Order >= 50) != (b.Order >= 50) {
			if a.Order >= 50 {
				return 1
			}
			return -1
		}
		if byTable := strings.Compare(a.Table, b.Table); byTable != 0 {
			return byTable
		}
		return a.Order - b.Order
	})
	var groups []schemaApplyGroup
	for _, change := range ordered {
		late := change.Order >= 50
		if len(groups) == 0 || groups[len(groups)-1].table != change.Table || groups[len(groups)-1].late != late {
			groups = append(groups, schemaApplyGroup{table: change.Table, late: late})
		}
		group := &groups[len(groups)-1]
		group.changes = append(group.changes, change)
	}
	return groups
}

func schemaResult(change diff.Change, status string, err error) schemaApplyResult {
	result := schemaApplyResult{ID: change.ID, Table: change.Table, Object: change.Object, Kind: change.Kind, Status: status}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func schemaDependencyFailed(change diff.Change, statuses map[string]string, provisional map[string]bool) bool {
	for _, dependency := range change.DependsOn {
		if statuses[dependency] != schemaApplied && !provisional[dependency] {
			return true
		}
	}
	return false
}

func schemaOutcomeUncertain(err error) bool {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (strings.HasPrefix(state.SQLState(), "08") || state.SQLState() == "40003") {
		return true
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 2006 || mysqlErr.Number == 2013 || mysqlErr.Number == 2055) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func logSchemaExecutionFailure(ctx context.Context, catalog diff.Catalog, table, stage string, err error) {
	var mysqlErr *mysql.MySQLError
	var sqlState interface{ SQLState() string }
	if schemaOutcomeUncertain(err) ||
		(errors.As(err, &mysqlErr) && mysqlErr.Number == 1205) ||
		(errors.As(err, &sqlState) && (sqlState.SQLState() == "55P03" || sqlState.SQLState() == "57014")) {
		slog.WarnContext(ctx, "schema operation interrupted or timed out", "target_product", catalog.Product,
			"target_namespace", catalog.Namespace, "table", table, "stage", stage)
	}
}

func executeSchemaChanges(ctx context.Context, db *sql.DB, catalog diff.Catalog, changes []diff.Change, observe func(schemaApplyResult)) []schemaApplyResult {
	groups := groupSchemaChanges(changes)
	lastGuardedGroup := make(map[string]int)
	for i, group := range groups {
		for _, name := range schemaGuardTableNames(catalog, group.table, group.changes) {
			lastGuardedGroup[name] = i
		}
	}
	statuses := make(map[string]string, len(changes))
	results := make([]schemaApplyResult, 0, len(changes))
	var stopErr error
	save := func(result schemaApplyResult) {
		statuses[result.ID] = result.Status
		results = append(results, result)
		if observe != nil {
			observe(result)
		}
		if result.Status == schemaUnknown {
			stopErr = errors.New("上一项执行结果未知，请重新比对")
		}
	}
	for i, group := range groups {
		if err := ctx.Err(); err != nil && stopErr == nil {
			logSchemaExecutionFailure(ctx, catalog, group.table, "before table group", err)
			stopErr = errors.New("应用请求已中断，请重新比对")
		}
		if stopErr != nil {
			for _, change := range group.changes {
				save(schemaResult(change, schemaSkipped, stopErr))
			}
			continue
		}
		if catalog.TransactionalDDL && source.IsPGFamily(catalog.Product) {
			for _, result := range executeSchemaPGGroup(ctx, db, &catalog, group, statuses) {
				save(result)
			}
			continue
		}
		selected, skipped := schemaGroupCandidates(group, statuses)
		for _, result := range skipped {
			save(result)
		}
		if len(selected) == 0 {
			continue
		}
		if catalog.Namespace != "" {
			names := schemaGuardTableNames(catalog, group.table, selected)
			if err := guardSchemaTargetCatalog(ctx, db, catalog, names, true); err != nil {
				logSchemaExecutionFailure(ctx, catalog, group.table, "read target catalog", err)
				for _, change := range selected {
					save(schemaResult(change, schemaFailed, err))
				}
				continue
			}
		}
		var groupErr error
		applied := false
		for _, change := range selected {
			switch {
			case groupErr != nil:
				save(schemaResult(change, schemaSkipped, groupErr))
			case schemaDependencyFailed(change, statuses, nil):
				save(schemaResult(change, schemaSkipped, errors.New("依赖差异未应用")))
			default:
				status, err := executeSchemaStatementChange(ctx, db, catalog, change)
				save(schemaResult(change, status, err))
				applied = applied || status == schemaApplied
				if err != nil {
					logSchemaExecutionFailure(ctx, catalog, group.table, "execute DDL", err)
					groupErr = errors.New("同表的上一项应用失败")
				}
			}
		}
		if catalog.Namespace != "" && applied && stopErr == nil && lastGuardedGroup[group.table] > i {
			// Later FK groups must see the shape produced by this request.
			updated, err := diff.ReadCatalog(ctx, db, catalog.Product, catalog.Namespace, []string{group.table})
			if err != nil || updated.Identity != catalog.Identity || updated.Tables[group.table] == nil {
				slog.WarnContext(ctx, "could not refresh schema after DDL", "target_product", catalog.Product,
					"target_namespace", catalog.Namespace, "table", group.table)
				stopErr = errors.New("应用后无法重新读取目标表，请重新比对")
				continue
			}
			refreshSchemaTargetTable(&catalog, updated, group.table)
		}
	}
	return results
}

func schemaGroupCandidates(group schemaApplyGroup, statuses map[string]string) ([]diff.Change, []schemaApplyResult) {
	// Earlier statements in the group can satisfy dependencies after execution.
	var selected []diff.Change
	provisional := make(map[string]bool, len(group.changes))
	var skipped []schemaApplyResult
	for _, change := range group.changes {
		if schemaDependencyFailed(change, statuses, provisional) {
			skipped = append(skipped, schemaResult(change, schemaSkipped, errors.New("依赖差异未应用")))
			continue
		}
		selected = append(selected, change)
		provisional[change.ID] = true
	}
	return selected, skipped
}

func executeSchemaPGGroup(ctx context.Context, db *sql.DB, catalog *diff.Catalog, group schemaApplyGroup, statuses map[string]string) []schemaApplyResult {
	selected, skipped := schemaGroupCandidates(group, statuses)
	if len(selected) == 0 {
		return skipped
	}
	failedAll := func(err error) []schemaApplyResult {
		logSchemaExecutionFailure(ctx, *catalog, group.table, "prepare or verify DDL transaction", err)
		for _, change := range selected {
			skipped = append(skipped, schemaResult(change, schemaFailed, err))
		}
		return skipped
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return failedAll(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return failedAll(err)
	}
	if err = ensureSchemaPGStringMode(ctx, tx, "SET LOCAL standard_conforming_strings = on"); err != nil {
		return failedAll(err)
	}
	if err := guardSchemaPGGroup(ctx, tx, catalog, group.table, selected); err != nil {
		return failedAll(err)
	}
	for i, change := range selected {
		if err = execSchemaStatements(ctx, tx, change.SQL); err == nil {
			continue
		}
		logSchemaExecutionFailure(ctx, *catalog, group.table, "execute DDL transaction", err)
		rollbackErr := tx.Rollback()
		status := schemaFailed
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			status = schemaUnknown
		}
		for j, item := range selected {
			switch {
			case j < i:
				skipped = append(skipped, schemaResult(item, status, errors.New("同表事务已回滚")))
			case j == i:
				skipped = append(skipped, schemaResult(item, status, err))
			default:
				skipped = append(skipped, schemaResult(item, schemaSkipped, errors.New("同表事务执行失败")))
			}
		}
		return skipped
	}
	updated, err := diff.ReadCatalog(ctx, tx, catalog.Product, catalog.Namespace, []string{group.table})
	if err != nil {
		return failedAll(err)
	}
	err = tx.Commit()
	if err != nil {
		logSchemaExecutionFailure(ctx, *catalog, group.table, "commit DDL transaction", err)
	}
	for _, change := range selected {
		if err != nil {
			skipped = append(skipped, schemaResult(change, schemaUnknown, err))
		} else {
			skipped = append(skipped, schemaResult(change, schemaApplied, nil))
		}
	}
	if err == nil {
		refreshSchemaTargetTable(catalog, updated, group.table)
	}
	return skipped
}

func schemaGuardTableNames(catalog diff.Catalog, table string, changes []diff.Change) []string {
	names := []string{table}
	for _, change := range changes {
		key, ok := change.Expected.(diff.Key)
		if ok && key.Kind == "foreign" && key.RefNamespace == catalog.Namespace && key.RefTable != "" {
			names = append(names, key.RefTable)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func guardSchemaPGGroup(ctx context.Context, tx *sql.Tx, catalog *diff.Catalog, table string, changes []diff.Change) error {
	names := schemaGuardTableNames(*catalog, table, changes)
	dia := dialect.NewPostgres(catalog.Product)
	for _, name := range names {
		// A new table doesn't have a relation to lock yet. Its plain CREATE
		// fails if it is occupied after this catalog read.
		if catalog.Tables[name] == nil {
			continue
		}
		mode := "SHARE ROW EXCLUSIVE"
		if name == table {
			mode = "ACCESS EXCLUSIVE"
		}
		qualified := dia.QualifyTable(catalog.Namespace, name)
		if _, err := tx.ExecContext(ctx, "LOCK TABLE "+qualified+" IN "+mode+" MODE"); err != nil {
			return err
		}
	}
	return guardSchemaTargetCatalog(ctx, tx, *catalog, names, false)
}

func guardSchemaTargetCatalog(ctx context.Context, q diff.Queryer, catalog diff.Catalog, names []string, checkSessionMode bool) error {
	current, err := diff.ReadCatalog(ctx, q, catalog.Product, catalog.Namespace, names)
	if err != nil {
		return err
	}
	if current.Identity != catalog.Identity {
		return errors.New("目标数据库身份已变化，请重新比对")
	}
	if current.CaseMode != catalog.CaseMode || current.IdentifierLimit != catalog.IdentifierLimit ||
		checkSessionMode && current.Mode != catalog.Mode {
		return errors.New("目标库兼容模式已变化，请重新比对")
	}
	for _, name := range names {
		if catalog.Occupied[name] != current.Occupied[name] || repair.Hash(catalog.Tables[name]) != repair.Hash(current.Tables[name]) {
			return fmt.Errorf("目标表 %q 结构已变化，请重新比对", name)
		}
	}
	return nil
}

func refreshSchemaTargetTable(catalog *diff.Catalog, updated diff.Catalog, table string) {
	catalog.Tables[table] = updated.Tables[table]
	catalog.Occupied[table] = updated.Occupied[table]
}

type schemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func execSchemaStatements(ctx context.Context, executor schemaExecutor, statements []string) error {
	for _, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const schemaMySQLDDLSQLMode = "SET SESSION sql_mode=CONCAT_WS(',', NULLIF(@@SESSION.sql_mode, ''), 'NO_BACKSLASH_ESCAPES', 'STRICT_ALL_TABLES')"
const schemaMySQLDDLCommentMode = "SET SESSION sql_mode=CONCAT_WS(',', NULLIF(?, ''), 'NO_BACKSLASH_ESCAPES', 'STRICT_ALL_TABLES')"

func ensureSchemaPGStringMode(ctx context.Context, session interface {
	schemaExecutor
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, setMode string) error {
	// Two backslashes are valid with either setting, but yield two bytes only
	// with SQL-standard literal handling. Some compatible products implement
	// that behavior without exposing the PostgreSQL GUC.
	const probe = `SELECT 'a\\b'`
	const expected = `a\\b`
	var found string
	if err := session.QueryRowContext(ctx, probe).Scan(&found); err != nil {
		return fmt.Errorf("核验 PG 字符串字面量: %w", err)
	}
	if found == expected {
		return nil
	}
	if _, err := session.ExecContext(ctx, setMode); err != nil {
		return fmt.Errorf("无法设置 PG 标准字符串模式: %w", err)
	}
	if err := session.QueryRowContext(ctx, probe).Scan(&found); err != nil {
		return fmt.Errorf("核验 PG 字符串字面量: %w", err)
	}
	if found != expected {
		return errors.New("目标库未启用可核验的 PG 标准字符串模式")
	}
	return nil
}

func executeSchemaStatementChange(ctx context.Context, db *sql.DB, catalog diff.Catalog, change diff.Change) (string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return schemaFailed, err
	}
	defer conn.Close()
	switch {
	case catalog.Product == "mysql":
		defer discardSchemaCompareSession(conn)
		if _, err = conn.ExecContext(ctx, "SET SESSION lock_wait_timeout=5"); err != nil {
			return schemaFailed, err
		}
		if change.Kind == "column_comment" {
			if err = guardSchemaMySQLColumnComment(ctx, conn, catalog.Namespace, change); err != nil {
				return schemaFailed, err
			}
			defer func() {
				unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _ = conn.ExecContext(unlockCtx, "UNLOCK TABLES")
			}()
			_, err = conn.ExecContext(ctx, schemaMySQLDDLCommentMode, catalog.Mode)
		} else {
			_, err = conn.ExecContext(ctx, schemaMySQLDDLSQLMode)
		}
	case source.IsPGFamily(catalog.Product):
		defer discardSchemaCompareSession(conn)
		err = ensureSchemaPGStringMode(ctx, conn, "SET SESSION standard_conforming_strings = on")
	}
	if err != nil {
		return schemaFailed, err
	}
	// These engines may commit each DDL statement on its own. Keep statuses
	// conservative if a multi-statement change stops after executing a prefix.
	for i, statement := range change.SQL {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			if i > 0 || schemaOutcomeUncertain(err) {
				return schemaUnknown, err
			}
			return schemaFailed, err
		}
	}
	return schemaApplied, nil
}

func guardSchemaMySQLColumnComment(ctx context.Context, conn *sql.Conn, namespace string, change diff.Change) error {
	expected, ok := change.Expected.(string)
	current, currentOK := change.Target.(string)
	if !ok || !currentOK || len(change.SQL) != 1 {
		return errors.New("目标列注释缺少可核验定义")
	}
	qualified := dialect.NewMySQL().QualifyTable(namespace, change.Table)
	if _, err := conn.ExecContext(ctx, "LOCK TABLES "+qualified+" WRITE"); err != nil {
		return err
	}
	// Preparing the candidate on the write-locked session prevents an
	// intervening column change from being overwritten by MODIFY COLUMN.
	statement, oldComment, err := inspectSchemaMySQLColumnComment(ctx, conn, namespace, change.Table, change.Object, expected)
	if err != nil {
		return err
	}
	if statement != change.SQL[0] || oldComment != current {
		return errors.New("目标列定义已变化，请重新比对")
	}
	return nil
}
