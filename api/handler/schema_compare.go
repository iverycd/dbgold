package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/target"
	"dbgold/diff"
	"dbgold/internal/pgschema"
	"dbgold/middleware"
	"dbgold/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	schemaCompareTTL      = 10 * time.Minute
	schemaCompareTimeout  = 90 * time.Second
	schemaApplyTimeout    = 180 * time.Second
	schemaCompareMaxPlans = 128
)

type schemaCompareEndpoint struct {
	ConnectionID uint   `json:"connection_id"`
	Namespace    string `json:"namespace"`
}

type schemaCompareRequest struct {
	Source       schemaCompareEndpoint `json:"source"`
	Target       schemaCompareEndpoint `json:"target"`
	Tables       []string              `json:"tables,omitempty"`
	PreserveCase bool                  `json:"preserve_case,omitempty"`
	CharInLength bool                  `json:"char_in_length,omitempty"`
	UseNvarchar2 bool                  `json:"use_nvarchar2,omitempty"`
}

type schemaComparePlan struct {
	ID           string        `json:"plan_id"`
	ExpiresAt    time.Time     `json:"expires_at"`
	Changes      []diff.Change `json:"changes"`
	owner        uint
	request      schemaCompareRequest
	sourceConfig string
	targetConfig string
	sourceDigest string
	targetDigest string
	targetScope  string
	targetSchema string
}

type schemaComparePlanCache struct {
	mu    sync.Mutex
	plans map[string]*schemaComparePlan
}

var schemaComparePlans = schemaComparePlanCache{plans: make(map[string]*schemaComparePlan)}

func (s *schemaComparePlanCache) save(plan *schemaComparePlan, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest *schemaComparePlan
	for id, current := range s.plans {
		if !now.Before(current.ExpiresAt) {
			delete(s.plans, id)
		} else if oldest == nil || current.ExpiresAt.Before(oldest.ExpiresAt) {
			oldest = current
		}
	}
	if len(s.plans) >= schemaCompareMaxPlans && oldest != nil {
		delete(s.plans, oldest.ID)
	}
	s.plans[plan.ID] = plan
}

func (s *schemaComparePlanCache) get(id string, owner uint, now time.Time, claim bool) *schemaComparePlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	if p == nil || p.owner != owner {
		return nil
	}
	if !now.Before(p.ExpiresAt) {
		delete(s.plans, id)
		return nil
	}
	if claim {
		delete(s.plans, id)
	}
	return p
}

func GetSchemaComparePairs(c *gin.Context) {
	c.JSON(http.StatusOK, diff.SupportedPairs())
}

func decodeSchemaCompare(c *gin.Context, request any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(request); err != nil || dec.Decode(new(any)) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "结构比对请求格式无效"})
		return false
	}
	return true
}

func validSchemaObjectName(value string) bool {
	return value != "" && !strings.ContainsRune(value, 0)
}

func validateSchemaCompareRequest(req *schemaCompareRequest) error {
	for _, endpoint := range []schemaCompareEndpoint{req.Source, req.Target} {
		if endpoint.ConnectionID == 0 || !validSchemaObjectName(endpoint.Namespace) {
			return errors.New("请选择源库和目标库连接及数据库 / Schema")
		}
	}
	for _, table := range req.Tables {
		if !validSchemaObjectName(table) {
			return errors.New("表名不能为空")
		}
	}
	slices.Sort(req.Tables)
	req.Tables = slices.Compact(req.Tables)
	if len(req.Tables) == 0 {
		req.Tables = nil
	}
	return nil
}

func ownedSchemaCompareConnections(c *gin.Context, req schemaCompareRequest) (*store.Connection, *store.Connection, bool) {
	owner, admin := middleware.GetCurrentUserID(c), middleware.IsAdmin(c)
	src, err := store.GetConnectionOwned(req.Source.ConnectionID, owner, admin)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "源库连接不存在"})
		return nil, nil, false
	}
	dst, err := store.GetConnectionOwned(req.Target.ConnectionID, owner, admin)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "目标库连接不存在"})
		return nil, nil, false
	}
	return src, dst, true
}

func schemaCompareNamespaceAllowed(product, namespace string) bool {
	lower := strings.ToLower(namespace)
	switch product {
	case "mysql":
		return !slices.Contains([]string{"mysql", "information_schema", "performance_schema", "sys"}, lower)
	case "dameng":
		return !slices.Contains([]string{"sys", "sysauditor", "syssso"}, lower)
	default:
		return !pgschema.IsHidden(namespace) && !strings.HasPrefix(lower, "pg_") && lower != "information_schema"
	}
}

func openSchemaCompareDB(conn *store.Connection, namespace string) (*sql.DB, error) {
	driverName := pgCompatibleDriverName(conn.DBType)
	selected := *conn
	switch conn.DBType {
	case "mysql":
		driverName = "mysql"
		selected.Database = namespace
	case "dameng":
		driverName = "dm"
	}
	return sql.Open(driverName, buildDSN(&selected))
}

func readSchemaCompareCatalogs(ctx context.Context, src, dst *sql.DB, srcConn, dstConn *store.Connection, req schemaCompareRequest) (diff.Catalog, diff.Catalog, error) {
	// The planner needs the whole inventory for mapped-name collisions,
	// target-only tables and foreign keys that reference unselected tables.
	sourceCatalog, err := diff.ReadCatalog(ctx, src, srcConn.DBType, req.Source.Namespace, nil)
	if err != nil {
		return diff.Catalog{}, diff.Catalog{}, fmt.Errorf("读取源库结构: %w", err)
	}
	targetCatalog, err := diff.ReadCatalog(ctx, dst, dstConn.DBType, req.Target.Namespace, nil)
	if err != nil {
		return diff.Catalog{}, diff.Catalog{}, fmt.Errorf("读取目标库结构: %w", err)
	}
	return sourceCatalog, targetCatalog, nil
}

func discardSchemaCompareSession(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func schemaCompareError(c *gin.Context, err error, conns ...*store.Connection) {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	c.JSON(status, gin.H{"error": schemaCompareErrorText(err, conns...)})
}

func schemaCompareErrorText(err error, conns ...*store.Connection) string {
	message := err.Error()
	for _, conn := range conns {
		if conn.Password != "" {
			message = strings.ReplaceAll(message, conn.Password, "***")
		}
	}
	return message
}

func schemaCompareScope(conn *store.Connection, catalog diff.Catalog) string {
	selected := *conn
	namespace := catalog.Namespace
	if conn.DBType == "mysql" {
		if catalog.CaseMode != 0 {
			namespace = strings.ToLower(namespace)
		}
		selected.Database = namespace
	} else if conn.DBType == "dameng" {
		// The DM connection's Database field is a saved owner, not a DSN
		// database. The selected owner alone determines the target scope.
		selected.Database = ""
	}
	return migrationScope(&selected, namespace)
}

func schemaConnectionDigest(conn *store.Connection) string {
	return repair.Hash([]any{conn.ID, conn.OwnerID, conn.DBType, conn.Host, conn.Port, conn.Database, conn.Username, conn.Password})
}

func CompareTableSchemas(c *gin.Context) {
	var req schemaCompareRequest
	if !decodeSchemaCompare(c, &req) {
		return
	}
	if err := validateSchemaCompareRequest(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	srcConn, dstConn, ok := ownedSchemaCompareConnections(c, req)
	if !ok {
		return
	}
	if !diff.SupportsPair(srcConn.DBType, dstConn.DBType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "暂不支持该数据库结构比对组合"})
		return
	}
	if !schemaCompareNamespaceAllowed(srcConn.DBType, req.Source.Namespace) || !schemaCompareNamespaceAllowed(dstConn.DBType, req.Target.Namespace) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择业务数据库 / Schema"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), schemaCompareTimeout)
	defer cancel()
	src, err := openSchemaCompareDB(srcConn, req.Source.Namespace)
	if err != nil {
		schemaCompareError(c, err, srcConn)
		return
	}
	defer src.Close()
	dst, err := openSchemaCompareDB(dstConn, req.Target.Namespace)
	if err != nil {
		schemaCompareError(c, err, dstConn)
		return
	}
	defer dst.Close()
	sourceCatalog, targetCatalog, err := readSchemaCompareCatalogs(ctx, src, dst, srcConn, dstConn, req)
	if err != nil {
		schemaCompareError(c, err, srcConn, dstConn)
		return
	}
	targetScope := schemaCompareScope(dstConn, targetCatalog)
	if schemaCompareScope(srcConn, sourceCatalog) == targetScope {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库和目标库必须为不同的数据库 / Schema"})
		return
	}
	options := diff.Options{
		Tables: req.Tables, PreserveCase: req.PreserveCase, CharInLength: req.CharInLength, UseNvarchar2: req.UseNvarchar2,
	}
	if dstConn.DBType == "mysql" {
		options.MySQLColumnComment = func(table, column, comment string) (string, error) {
			conn, err := dst.Conn(ctx)
			if err != nil {
				return "", err
			}
			defer discardSchemaCompareSession(conn)
			statement, _, err := inspectSchemaMySQLColumnComment(ctx, conn, targetCatalog.Namespace, table, column, comment)
			return statement, err
		}
	}
	changes := diff.Build(sourceCatalog, targetCatalog, options)
	if err := ctx.Err(); err != nil {
		schemaCompareError(c, err, srcConn, dstConn)
		return
	}
	if changes == nil {
		changes = []diff.Change{}
	}
	numberSchemaExecutionOrder(changes)
	now := time.Now()
	plan := &schemaComparePlan{
		ID: uuid.NewString(), ExpiresAt: now.Add(schemaCompareTTL), Changes: changes,
		owner: middleware.GetCurrentUserID(c), request: req,
		sourceConfig: schemaConnectionDigest(srcConn), targetConfig: schemaConnectionDigest(dstConn),
		sourceDigest: repair.Hash(sourceCatalog), targetDigest: repair.Hash(targetCatalog),
		targetScope: targetScope, targetSchema: targetCatalog.Namespace,
	}
	schemaComparePlans.save(plan, now)
	c.JSON(http.StatusOK, plan)
}

func numberSchemaExecutionOrder(changes []diff.Change) {
	indexes := make(map[string]int, len(changes))
	for i, change := range changes {
		indexes[change.ID] = i
	}
	order := 0
	for _, group := range groupSchemaChanges(changes) {
		for _, change := range group.changes {
			if change.Status == diff.Applicable {
				order++
				changes[indexes[change.ID]].ExecutionOrder = order
			}
		}
	}
}

func inspectSchemaMySQLColumnComment(ctx context.Context, conn *sql.Conn, namespace, table, column, comment string) (string, string, error) {
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode='', sql_quote_show_create=1"); err != nil {
		return "", "", err
	}
	return target.InspectMySQLColumnComment(ctx, conn, namespace, table, column, comment)
}

func logSchemaCompareResult(plan *schemaComparePlan, result schemaApplyResult) {
	slog.Info("schema change outcome", "plan_id", plan.ID, "owner_id", plan.owner,
		"target_connection_id", plan.request.Target.ConnectionID, "target_namespace", plan.request.Target.Namespace,
		"change_id", result.ID, "table", result.Table, "object", result.Object, "kind", result.Kind, "status", result.Status)
}
