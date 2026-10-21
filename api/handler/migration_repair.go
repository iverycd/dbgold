package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/middleware"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func repairJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func repairError(c *gin.Context, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, store.ErrRepairConflict) {
		code = http.StatusConflict
	}
	c.JSON(code, gin.H{"error": err.Error()})
}
func ownedRepairJob(c *gin.Context, write bool) (*store.DataMigrationJob, bool) {
	j, e := store.GetDataMigrationJob(c.Param("jobID"))
	if e != nil || (!middleware.IsAdmin(c) && j.OwnerID != middleware.GetCurrentUserID(c)) {
		c.JSON(404, gin.H{"error": "任务不存在"})
		return nil, false
	}
	if write && j.Status == "running" {
		repairError(c, fmt.Errorf("原迁移仍在运行，暂不能修复"))
		return nil, false
	}
	return j, true
}
func connectionFingerprint(c *store.Connection) string {
	return repair.Hash([]any{c.DBType, strings.ToLower(c.Host), c.Port, c.Database, c.Username})
}
func migrationScope(c *store.Connection, schema string) string {
	family := c.DBType
	if source.IsPGFamily(family) {
		family = "postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addresses, _ := net.DefaultResolver.LookupIPAddr(ctx, c.Host)
	return repair.Hash([]any{family, canonicalMigrationHost(c.Host, addresses), c.Port, c.Database, schema})
}
func canonicalMigrationHost(host string, addresses []net.IPAddr) string {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	}
	unique := map[string]bool{}
	for _, a := range addresses {
		if a.IP.IsLoopback() {
			unique["loopback"] = true
		} else {
			unique[a.IP.String()] = true
		}
	}
	if len(unique) == 0 {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if host == "localhost" {
			return "loopback"
		}
		return host
	}
	values := make([]string, 0, len(unique))
	for value := range unique {
		values = append(values, value)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}
func migrationSourceLocation(j *store.DataMigrationJob, c *store.Connection, override string) {
	j.SourceDatabase = c.Database
	j.SourceConnectionDatabase = c.Database
	if source.IsPGFamily(c.DBType) {
		j.SourceSchema = override
		if j.SourceSchema == "" {
			j.SourceSchema = "public"
		}
	} else if c.DBType == "oracle" || c.DBType == "dameng" {
		j.SourceSchema = c.Database
		if override != "" {
			j.SourceSchema = override
		}
		// Match the effective owner used by the existing Reader constructors.
		j.SourceSchema = strings.ToUpper(j.SourceSchema)
	} else if override != "" {
		j.SourceDatabase = override
	}
	if c.DBType == "sqlserver" && j.DstDBType == "dameng" {
		j.SourceSchema = "dbo"
	}
}
func sourceOverride(b *store.MigrationRepairBinding, typ string) string {
	if source.IsPGFamily(typ) || typ == "oracle" || typ == "dameng" {
		return b.SourceSchema
	}
	return b.SourceDatabase
}

// Recover only the frozen source scope; the selected connection's default must
// never replace the owner used by the original migration.
func fillRepairSourceSchema(j *store.DataMigrationJob, b *store.MigrationRepairBinding) {
	if b.SourceSchema != "" {
		return
	}
	b.SourceSchema = j.SourceSchema
	if b.SourceSchema == "" && j.RepairConfigJSON == "" && j.SrcDBType == "dameng" {
		// Legacy Dameng snapshots stored the effective owner in SrcConnDatabase.
		b.SourceSchema = strings.ToUpper(j.SrcConnDatabase)
	}
}

func initialRepairBinding(j *store.DataMigrationJob, cfgJSON string, legacy bool) *store.MigrationRepairBinding {
	schema := j.SourceSchema
	if !legacy && schema == "" && source.IsPGFamily(j.SrcDBType) {
		schema = "public"
	}
	b := &store.MigrationRepairBinding{JobID: j.JobID, SrcConnID: j.SrcConnID, DstConnID: j.DstConnID, SourceDatabase: j.SourceDatabase, SourceSchema: schema, TargetSchema: j.DstSchema, ConfigJSON: cfgJSON, Configured: cfgJSON != "", Legacy: legacy, Version: 1}
	fillRepairSourceSchema(j, b)
	return b
}
func persistMigrationManifest(j *store.DataMigrationJob, src, dst *store.Connection, m datamigrate.RepairManifest) error {
	cfg := m.Config
	cfg.RepairSink = nil
	cfg.RepairObjectSink = nil
	cfg.CaptureRepair = false
	b := initialRepairBinding(j, repairJSON(cfg), false)
	b.SourceFingerprint = connectionFingerprint(src)
	b.TargetFingerprint = connectionFingerprint(dst)
	b.TargetSchema = cfg.TargetSchema
	rows := make([]store.MigrationRepairObject, 0, len(m.Objects))
	for _, o := range m.Objects {
		rows = append(rows, store.MigrationRepairObject{JobID: j.JobID, ObjectID: o.ID, ObjectJSON: repairJSON(o), Version: 1})
	}
	return store.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}}, DoUpdates: clause.AssignmentColumns([]string{"config_json", "target_schema"})}).Create(b).Error; e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		// Five bound columns per row keep each batch below the conservative
		// SQLite limit of 999. Conflicts preserve the row ID and draft version.
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}, {Name: "object_id"}}, DoUpdates: clause.AssignmentColumns([]string{"object_json", "updated_at"})}).CreateInBatches(&rows, 100).Error
	})
}

// Progress updates persist only the changed object. Binding/configuration and
// the complete inventory are saved by the initial and final manifest writes.
func persistMigrationRepairObject(jobID string, o repair.Object) error {
	return upsertMigrationRepairObject(store.DB, jobID, o)
}

func upsertMigrationRepairObject(db *gorm.DB, jobID string, o repair.Object) error {
	row := store.MigrationRepairObject{JobID: jobID, ObjectID: o.ID, ObjectJSON: repairJSON(o), Version: 1}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}, {Name: "object_id"}}, DoUpdates: clause.AssignmentColumns([]string{"object_json", "updated_at"})}).Create(&row).Error
}

func ensureRepairLedger(j *store.DataMigrationJob) error {
	if _, e := store.GetMigrationRepairBinding(j.JobID); e == nil {
		return nil
	} else if !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	r, e := store.GetDataMigrationReport(j.JobID)
	if e != nil {
		return fmt.Errorf("此任务没有可恢复的迁移报告")
	}
	var report datamigrate.MigrationReport
	if e = json.Unmarshal([]byte(r.ReportJSON), &report); e != nil {
		return e
	}
	objects := datamigrate.LegacyRepairObjects(report, j.DstSchema)
	cfg := j.RepairConfigJSON
	legacy := true
	if report.RepairManifest != nil {
		objects = report.RepairManifest.Objects
		reportConfig := report.RepairManifest.Config
		reportConfig.RepairSink = nil
		reportConfig.RepairObjectSink = nil
		cfg = repairJSON(reportConfig)
		legacy = false
	}
	datamigrate.EnrichRepairReferences(objects, report)
	b := initialRepairBinding(j, cfg, legacy)
	rows := make([]store.MigrationRepairObject, 0, len(objects))
	for _, o := range objects {
		o.NormalizeLegacyExecution()
		rows = append(rows, store.MigrationRepairObject{JobID: j.JobID, ObjectID: o.ID, ObjectJSON: repairJSON(o), Version: 1})
	}
	return store.SeedMigrationRepairs(b, rows)
}
func repairObjects(jobID string) ([]repair.Object, []store.MigrationRepairObject, error) {
	rows, e := store.ListMigrationRepairObjects(jobID)
	if e != nil {
		return nil, nil, e
	}
	objects := make([]repair.Object, 0, len(rows))
	for _, r := range rows {
		var o repair.Object
		if e = json.Unmarshal([]byte(r.ObjectJSON), &o); e != nil {
			return nil, nil, e
		}
		objects = append(objects, o)
	}
	enrichRepairReferences(jobID, objects)
	return objects, rows, nil
}

func enrichRepairReferences(jobID string, objects []repair.Object) {
	r, err := store.GetDataMigrationReport(jobID)
	if err != nil {
		return
	}
	var report datamigrate.MigrationReport
	if json.Unmarshal([]byte(r.ReportJSON), &report) == nil {
		datamigrate.EnrichRepairReferences(objects, report)
	}
}
func loadRepairObject(c *gin.Context, write bool) (*store.DataMigrationJob, *store.MigrationRepairBinding, *store.MigrationRepairObject, repair.Object, bool) {
	j, ok := ownedRepairJob(c, write)
	if !ok {
		return nil, nil, nil, repair.Object{}, false
	}
	if e := ensureRepairLedger(j); e != nil {
		repairError(c, e)
		return nil, nil, nil, repair.Object{}, false
	}
	b, e := store.GetMigrationRepairBinding(j.JobID)
	if e != nil {
		repairError(c, e)
		return nil, nil, nil, repair.Object{}, false
	}
	fillRepairSourceSchema(j, b)
	row, e := store.GetMigrationRepairObject(j.JobID, c.Param("objectID"))
	if e != nil {
		c.JSON(404, gin.H{"error": "对象不存在"})
		return nil, nil, nil, repair.Object{}, false
	}
	var o repair.Object
	if e = json.Unmarshal([]byte(row.ObjectJSON), &o); e != nil {
		repairError(c, e)
		return nil, nil, nil, repair.Object{}, false
	}
	objects := []repair.Object{o}
	enrichRepairReferences(j.JobID, objects)
	return j, b, row, objects[0], true
}
func bindingResponse(b *store.MigrationRepairBinding, j *store.DataMigrationJob) gin.H {
	return gin.H{"src_conn_id": b.SrcConnID, "dst_conn_id": b.DstConnID, "source_database": b.SourceDatabase, "source_schema": b.SourceSchema, "target_schema": b.TargetSchema, "configured": b.Configured, "legacy": b.Legacy, "src_db_type": j.SrcDBType, "dst_db_type": j.DstDBType}
}
func ListMigrationRepairs(c *gin.Context) {
	j, ok := ownedRepairJob(c, false)
	if !ok {
		return
	}
	if e := ensureRepairLedger(j); e != nil {
		repairError(c, e)
		return
	}
	objects, rows, e := repairObjects(j.JobID)
	if e != nil {
		repairError(c, e)
		return
	}
	out := make([]gin.H, 0, len(objects))
	for i, o := range objects {
		out = append(out, gin.H{"object": o, "version": rows[i].Version})
	}
	b, e := store.GetMigrationRepairBinding(j.JobID)
	if e != nil {
		repairError(c, e)
		return
	}
	fillRepairSourceSchema(j, b)
	var a store.MigrationRepairAttempt
	store.DB.Where("job_id = ? AND status = ?", j.JobID, "running").First(&a)
	c.JSON(200, gin.H{"objects": out, "binding": bindingResponse(b, j), "running_attempt_id": a.ID})
}
func GetMigrationRepair(c *gin.Context) {
	_, _, row, o, ok := loadRepairObject(c, false)
	if ok {
		c.JSON(200, gin.H{"object": o, "version": row.Version})
	}
}

// Snapshot comparisons intentionally ignore display names and credentials, but never endpoints or principals.
func checkRepairConnection(j *store.DataMigrationJob, c *store.Connection, src bool) error {
	typ, host, port, db, user := j.DstDBType, j.DstConnHost, j.DstConnPort, j.DstConnDatabase, j.DstConnUsername
	checkDatabase := db != ""
	role := "目标连接"
	if src {
		role = "源连接"
		typ, host, port, user = j.SrcDBType, j.SrcConnHost, j.SrcConnPort, j.SrcConnUsername
		db = j.SourceConnectionDatabase
		// The repair config and source location were captured together. An empty
		// connection database is therefore a real snapshot value, not missing data.
		checkDatabase = j.RepairConfigJSON != "" || db != ""
		if !checkDatabase && !source.IsPGFamily(typ) && typ != "dameng" {
			db = j.SrcConnDatabase
			checkDatabase = db != ""
		}
	}
	var mismatches []string
	if c.DBType != typ {
		mismatches = append(mismatches, fmt.Sprintf("数据库类型（原任务 %q，当前连接 %q）", typ, c.DBType))
	}
	if host != "" && !strings.EqualFold(host, c.Host) {
		mismatches = append(mismatches, fmt.Sprintf("主机（原任务 %q，当前连接 %q）", host, c.Host))
	}
	if port != 0 && port != c.Port {
		mismatches = append(mismatches, fmt.Sprintf("端口（原任务 %d，当前连接 %d）", port, c.Port))
	}
	if checkDatabase && db != c.Database {
		mismatches = append(mismatches, fmt.Sprintf("数据库（原任务 %q，当前连接 %q）", db, c.Database))
	}
	if user != "" && user != c.Username {
		mismatches = append(mismatches, fmt.Sprintf("账号（原任务 %q，当前连接 %q）", user, c.Username))
	}
	if len(mismatches) != 0 {
		return fmt.Errorf("%s与原任务不匹配：%s", role, strings.Join(mismatches, "；"))
	}
	return nil
}
func UpdateMigrationRepairBinding(c *gin.Context) {
	j, ok := ownedRepairJob(c, true)
	if !ok {
		return
	}
	if e := ensureRepairLedger(j); e != nil {
		repairError(c, e)
		return
	}
	var req struct {
		SrcConnID      uint                `json:"src_conn_id"`
		DstConnID      uint                `json:"dst_conn_id"`
		Config         *datamigrate.Config `json:"config"`
		SourceDatabase string              `json:"source_database"`
		SourceSchema   string              `json:"source_schema"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		repairError(c, e)
		return
	}
	b, e := store.GetMigrationRepairBinding(j.JobID)
	if e != nil {
		repairError(c, e)
		return
	}
	fillRepairSourceSchema(j, b)
	dst, e := store.GetConnectionOwned(req.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil {
		c.JSON(404, gin.H{"error": "目标连接不存在"})
		return
	}
	if e = checkRepairConnection(j, dst, false); e != nil {
		repairError(c, e)
		return
	}
	var src *store.Connection
	if req.SrcConnID != 0 {
		src, e = store.GetConnectionOwned(req.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
		if e != nil {
			c.JSON(404, gin.H{"error": "源连接不存在"})
			return
		}
		if e = checkRepairConnection(j, src, true); e != nil {
			repairError(c, e)
			return
		}
	}
	oldVersion := b.Version
	b.SrcConnID = req.SrcConnID
	b.DstConnID = req.DstConnID
	b.TargetFingerprint = connectionFingerprint(dst)
	if src != nil {
		b.SourceFingerprint = connectionFingerprint(src)
		if b.SourceDatabase == "" {
			b.SourceDatabase = src.Database
		}
		if req.SourceDatabase != "" && req.SourceDatabase != b.SourceDatabase {
			repairError(c, fmt.Errorf("源数据库与原任务不一致"))
			return
		}
	}
	if req.SourceSchema != "" {
		if b.SourceSchema != req.SourceSchema && (!b.Legacy || b.SourceSchema != "") {
			repairError(c, fmt.Errorf("不能变更原任务源 Schema"))
			return
		}
		b.SourceSchema = req.SourceSchema
	}
	if src != nil && b.SourceSchema == "" && (source.IsPGFamily(src.DBType) || src.DBType == "oracle" || src.DBType == "dameng") {
		repairError(c, fmt.Errorf("历史任务缺少源Schema，请显式补齐原迁移范围"))
		return
	}
	if req.Config != nil {
		if !b.Legacy && b.Configured {
			repairError(c, fmt.Errorf("新任务配置已固定，不能覆盖"))
			return
		}
		cfg := *req.Config
		cfg.MigrateTriggers = cfg.MigrateTriggers || j.MigrateTriggers
		if cfg.Content != "both" && cfg.Content != "schema_only" && cfg.Content != "data_only" {
			repairError(c, fmt.Errorf("请显式补齐原迁移内容"))
			return
		}
		if err := validateRepairTriggerConfig(j, cfg); err != nil {
			repairError(c, err)
			return
		}
		if cfg.TargetSchema != "" && b.TargetSchema != "" && cfg.TargetSchema != b.TargetSchema {
			repairError(c, fmt.Errorf("不能变更目标 Schema"))
			return
		}
		if b.TargetSchema == "" {
			b.TargetSchema = cfg.TargetSchema
		}
		cfg.TargetSchema = b.TargetSchema
		cfg.TargetDBType = j.DstDBType
		cfg.CaptureRepair = false
		cfg.RepairSink = nil
		cfg.RepairObjectSink = nil
		b.ConfigJSON = repairJSON(cfg)
		b.Configured = true
	}
	if b.TargetSchema == "" {
		repairError(c, fmt.Errorf("旧任务缺少明确的目标 Schema，请补齐原配置后绑定"))
		return
	}
	b.Version++
	e = store.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&store.MigrationRepairAttempt{}).Where("job_id = ? AND status = ?", j.JobID, "running").Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return store.ErrRepairConflict
		}
		r := tx.Model(&store.MigrationRepairBinding{}).Where("job_id = ? AND version = ?", j.JobID, oldVersion).Select("*").Updates(b)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return store.ErrRepairConflict
		}
		var objects []store.MigrationRepairObject
		if err := tx.Where("job_id = ?", j.JobID).Find(&objects).Error; err != nil {
			return err
		}
		for _, row := range objects {
			var obj repair.Object
			if err := json.Unmarshal([]byte(row.ObjectJSON), &obj); err != nil {
				return err
			}
			changed := false
			if obj.Schema == "" {
				obj.Schema = b.TargetSchema
				obj.RequiresReview = true
				changed = true
			}
			if obj.Legacy && b.Configured && (obj.Kind == "table" || obj.Kind == "data") {
				var cfg datamigrate.Config
				if err := json.Unmarshal([]byte(b.ConfigJSON), &cfg); err != nil {
					return err
				}
				name := obj.SourceName
				if j.DstDBType == "oracle" || j.DstDBType == "oscar" || j.DstDBType == "dameng" && (j.SrcDBType == "sqlserver" || j.SrcDBType == "oracle") {
					name = strings.ToUpper(name)
				} else if cfg.LowerCaseNames {
					name = strings.ToLower(name)
				}
				if obj.TargetName != name || obj.Table != name {
					obj.TargetName = name
					obj.Table = name
					changed = true
				}
			}
			if changed {
				if err := tx.Model(&row).Updates(map[string]any{"object_json": repairJSON(obj), "version": row.Version + 1}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if e != nil {
		repairError(c, e)
		return
	}
	c.JSON(200, bindingResponse(b, j))
}
func resolveRepairConnections(c *gin.Context, j *store.DataMigrationJob, b *store.MigrationRepairBinding, needSource bool) (*store.Connection, *store.Connection, error) {
	dst, e := store.GetConnectionOwned(b.DstConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil {
		return nil, nil, fmt.Errorf("请绑定有权限的原目标连接")
	}
	if e = checkRepairConnection(j, dst, false); e != nil {
		return nil, nil, e
	}
	if b.TargetFingerprint != "" && b.TargetFingerprint != connectionFingerprint(dst) {
		return nil, nil, fmt.Errorf("目标连接已变化，请重新绑定")
	}
	if b.TargetSchema == "" {
		return nil, nil, fmt.Errorf("请先绑定明确的原目标 Schema")
	}
	if !needSource {
		return nil, dst, nil
	}
	src, e := store.GetConnectionOwned(b.SrcConnID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if e != nil {
		return nil, nil, fmt.Errorf("请绑定有权限的原源库连接")
	}
	if e = checkRepairConnection(j, src, true); e != nil {
		return nil, nil, e
	}
	if b.SourceFingerprint != "" && b.SourceFingerprint != connectionFingerprint(src) {
		return nil, nil, fmt.Errorf("源连接已变化，请重新绑定")
	}
	return src, dst, nil
}

func validateRepairUnits(units []repair.Unit) error {
	if len(units) > 100 {
		return fmt.Errorf("单对象执行单元不能超过100个")
	}
	selected := 0
	for _, u := range units {
		if !u.Selected || u.Status == "succeeded" {
			continue
		}
		selected++
		if strings.TrimSpace(u.SQL) == "" || len(u.SQL) > 1<<20 {
			return fmt.Errorf("执行单元不能为空且不能超过1 MiB")
		}
		tokens, _, e := scanQueryTokens(u.SQL)
		if e != nil {
			return e
		}
		if len(tokens) == 0 {
			return fmt.Errorf("SQL不能只有注释")
		}
		switch tokens[0] {
		case "CREATE", "ALTER", "COMMENT", "DECLARE", "BEGIN", "IF", "DO", "EXEC", "EXECUTE", "DBCC", "DROP":
		default:
			return fmt.Errorf("修复仅接受对象 DDL 执行单元")
		}
		for i, t := range tokens {
			if t == "TRUNCATE" || t == "USE" {
				return fmt.Errorf("对象修复禁止清空表或切换数据库")
			}
			if i+1 < len(tokens) {
				n := tokens[i+1]
				if t == "DROP" && (n == "TABLE" || n == "DATABASE" || n == "SCHEMA") {
					return fmt.Errorf("对象修复禁止删除表、数据库或 Schema")
				}
				if t == "ALTER" && (n == "SESSION" || n == "DATABASE" || n == "SYSTEM") {
					return fmt.Errorf("对象修复禁止修改连接范围")
				}
				if t == "SET" && (n == "SEARCH_PATH" || n == "SCHEMA") {
					return fmt.Errorf("对象修复禁止切换 Schema")
				}
			}
		}
	}
	if selected == 0 {
		return fmt.Errorf("请选择至少一个未成功的执行单元")
	}
	return nil
}
func SaveMigrationRepairDraft(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	j, binding, row, o, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	if !o.CanEditDDL() {
		repairError(c, fmt.Errorf("此记录仅供诊断或接续数据，不支持编辑对象 DDL"))
		return
	}
	var req struct {
		Version       int               `json:"version"`
		Units         []repair.Unit     `json:"units"`
		ColumnMapping map[string]string `json:"column_mapping"`
		Reviewed      bool              `json:"reviewed"`
		SequencePlan  bool              `json:"sequence_plan"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		repairError(c, e)
		return
	}
	if req.Version != row.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	if req.SequencePlan {
		if !req.Reviewed {
			repairError(c, fmt.Errorf("请先核对序列计划并确认完整执行单元"))
			return
		}
		candidate, _, err := migrationSequencePlan(c, j, binding, o, row.Version)
		if err != nil {
			repairError(c, err)
			return
		}
		if err := validateSequencePlanDraft(candidate, req.Units); err != nil {
			repairError(c, err)
			return
		}
		o = candidate
	}
	if o.Status == "unknown" {
		repairError(c, fmt.Errorf("执行结果未知，不允许自动重放"))
		return
	}
	existing := map[string]repair.Unit{}
	for _, u := range o.Units {
		if u.Status == "unknown" || u.Status == "running" {
			repairError(c, fmt.Errorf("存在执行结果未知的单元，不允许自动重放"))
			return
		}
		existing[u.ID] = u
	}
	seen := map[string]bool{}
	out := make([]repair.Unit, 0, len(req.Units))
	for _, u := range req.Units {
		if u.ID == "" {
			u.ID = uuid.NewString()
		}
		if seen[u.ID] {
			repairError(c, fmt.Errorf("重复执行单元"))
			return
		}
		seen[u.ID] = true
		if old, exists := existing[u.ID]; exists {
			if old.Status == "running" || old.Status == "unknown" {
				repairError(c, fmt.Errorf("执行结果未知，不允许自动重放该对象"))
				return
			}
			if old.Status == "succeeded" && u.SQL != old.SQL {
				repairError(c, fmt.Errorf("成功步骤不可修改或再次执行"))
				return
			}
			old.SQL = u.SQL
			old.Selected = u.Selected && old.Status != "succeeded"
			u = old
		} else {
			u.OriginalSQL = ""
			u.Status = "pending"
			u.Error = ""
		}
		out = append(out, u)
	}
	for _, u := range o.Units {
		if u.Status == "succeeded" && !seen[u.ID] {
			repairError(c, fmt.Errorf("不可删除成功执行单元"))
			return
		}
	}
	if len(out) > 100 {
		repairError(c, fmt.Errorf("执行单元过多"))
		return
	}
	sqlChanged := len(o.Units) != len(out)
	for i, u := range out {
		if i >= len(o.Units) || u.ID != o.Units[i].ID || u.SQL != o.Units[i].SQL {
			sqlChanged = true
		}
	}
	o.Units = out
	if req.ColumnMapping != nil {
		o.ColumnMapping = req.ColumnMapping
	}
	if req.Reviewed {
		o.RequiresReview = false
	}
	if sqlChanged || o.Status != "succeeded" {
		o.Status = "draft"
	}
	if e := store.SaveMigrationRepairObject(row.JobID, row.ObjectID, req.Version, repairJSON(o)); e != nil {
		repairError(c, e)
		return
	}
	c.JSON(200, gin.H{"object": o, "version": req.Version + 1})
}

var repairCancels sync.Map

func attemptResponse(a *store.MigrationRepairAttempt) gin.H {
	var units []repair.Unit
	_ = json.Unmarshal([]byte(a.UnitsJSON), &units)
	if units == nil {
		units = []repair.Unit{}
	}
	var result any
	var verification any
	if a.VerificationJSON != "" {
		_ = json.Unmarshal([]byte(a.VerificationJSON), &verification)
	}
	if a.ResultJSON != "" {
		_ = json.Unmarshal([]byte(a.ResultJSON), &result)
	}
	return gin.H{"id": a.ID, "job_id": a.JobID, "owner_id": a.OwnerID, "object_id": a.ObjectID, "kind": a.Kind, "status": a.Status, "units": units, "result": result, "verification": verification, "error": a.Error, "created_at": a.CreatedAt, "finished_at": a.FinishedAt, "cancel_requested": a.CancelRequested}
}
func ListMigrationRepairAttempts(c *gin.Context) {
	j, ok := ownedRepairJob(c, false)
	if !ok {
		return
	}
	var rows []store.MigrationRepairAttempt
	query := store.DB.Where("job_id = ?", j.JobID)
	if id := c.Param("objectID"); id != "" {
		query = query.Where("object_id = ?", id)
	}
	if e := query.Order("created_at DESC").Find(&rows).Error; e != nil {
		repairError(c, e)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for i := range rows {
		out = append(out, attemptResponse(&rows[i]))
	}
	c.JSON(200, gin.H{"items": out})
}
func ownedRepairAttempt(c *gin.Context) (*store.MigrationRepairAttempt, bool) {
	j, ok := ownedRepairJob(c, false)
	if !ok {
		return nil, false
	}
	var a store.MigrationRepairAttempt
	if e := store.DB.First(&a, "job_id = ? AND id = ?", j.JobID, c.Param("attemptID")).Error; e != nil {
		c.JSON(404, gin.H{"error": "执行记录不存在"})
		return nil, false
	}
	return &a, true
}
func GetMigrationRepairAttempt(c *gin.Context) {
	if a, ok := ownedRepairAttempt(c); ok {
		c.JSON(200, attemptResponse(a))
	}
}
func CancelMigrationRepairAttempt(c *gin.Context) {
	a, ok := ownedRepairAttempt(c)
	if !ok {
		return
	}
	if a.Status == "running" {
		if e := store.DB.Model(a).Update("cancel_requested", true).Error; e != nil {
			repairError(c, e)
			return
		}
		if v, ok := repairCancels.Load(a.ID); ok {
			v.(context.CancelFunc)()
		}
	}
	c.JSON(200, gin.H{"message": "已记录取消请求；已提交变更会保留"})
}
func existingRepairRequest(c *gin.Context, jobID, requestID, hash string) bool {
	var a store.MigrationRepairAttempt
	if e := store.DB.First(&a, "job_id = ? AND request_id = ?", jobID, requestID).Error; e == nil {
		if a.RequestHash != hash {
			repairError(c, store.ErrRepairConflict)
		} else {
			c.JSON(202, attemptResponse(&a))
		}
		return true
	}
	return false
}
func startRepairWorker(a *store.MigrationRepairAttempt, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	repairCancels.Store(a.ID, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer repairCancels.Delete(a.ID)
		defer func() {
			if x := recover(); x != nil {
				a.Status = "unknown"
				a.Error = fmt.Sprintf("执行中断，结果未知: %v", x)
				now := time.Now()
				a.FinishedAt = &now
				payload := ""
				if row, err := store.GetMigrationRepairObject(a.JobID, a.ObjectID); err == nil {
					var o repair.Object
					if json.Unmarshal([]byte(row.ObjectJSON), &o) == nil {
						if a.Kind == "ddl" {
							o.Status = "unknown"
						} else {
							var result repair.RecoveryResult
							_ = json.Unmarshal([]byte(a.ResultJSON), &result)
							if result.DataStatus != "succeeded" {
								o.DataStatus = "unknown"
							}
						}
						for i := range o.Units {
							if o.Units[i].Status == "running" {
								o.Units[i].Status = "unknown"
							}
						}
						payload = repairJSON(o)
					}
				}
				_ = store.FinishRepairAttempt(a, payload)
			}
		}()
		var current store.MigrationRepairAttempt
		if store.DB.First(&current, "id = ?", a.ID).Error == nil && current.CancelRequested {
			cancel()
		}
		fn(ctx)
	}()
}
func finishRepair(a *store.MigrationRepairAttempt, o repair.Object, err error) {
	if err != nil {
		a.Error = err.Error()
		if a.Status == "running" {
			a.Status = "failed"
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			a.Status = "unknown"
		}
		var units []repair.Unit
		_ = json.Unmarshal([]byte(a.UnitsJSON), &units)
		for _, u := range units {
			if u.Status == "running" || u.Status == "unknown" {
				a.Status = "unknown"
			}
		}
	} else if a.Status == "running" {
		a.Status = "succeeded"
	}
	if a.Kind == "ddl" {
		o.Status = a.Status
		o.Verification = nil
		o.ManualVerification = nil
		a.VerificationJSON = ""
		o.Units = nil
		_ = json.Unmarshal([]byte(a.UnitsJSON), &o.Units)
	}
	now := time.Now()
	a.FinishedAt = &now
	if e := store.FinishRepairAttempt(a, repairJSON(o)); e != nil {
		slog.Error("persist repair completion failed", "attempt", a.ID, "err", e)
	}
}
func ExecuteMigrationRepair(c *gin.Context) {
	j, b, row, o, ok := loadRepairObject(c, true)
	if !ok {
		return
	}
	if !o.CanExecuteDDL() {
		repairError(c, fmt.Errorf("此记录不是可执行 DDL 的目标对象"))
		return
	}
	var req struct {
		Version   int    `json:"version"`
		RequestID string `json:"request_id"`
		Confirmed bool   `json:"confirmed"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		repairError(c, e)
		return
	}
	hash := repair.Hash([]any{"ddl", o.ID, req.Version, req.Confirmed})
	if req.RequestID == "" || !req.Confirmed {
		repairError(c, fmt.Errorf("请审阅完整 SQL 并确认执行"))
		return
	}
	if existingRepairRequest(c, j.JobID, req.RequestID, hash) {
		return
	}
	if req.Version != row.Version {
		repairError(c, store.ErrRepairConflict)
		return
	}
	if o.Status == "unknown" {
		repairError(c, fmt.Errorf("执行结果未知，不允许自动重放"))
		return
	}
	if o.MySQLPGTrigger != nil {
		// The read-only retry action always requests the whole frozen transaction;
		// it does not expose or depend on persisted per-unit selection controls.
		for i := range o.Units {
			if o.Units[i].Status != "succeeded" {
				o.Units[i].Selected = true
			}
		}
	}
	if o.RequiresReview && o.MySQLPGTrigger == nil {
		repairError(c, fmt.Errorf("请先审阅并保存执行单元"))
		return
	}
	for _, u := range o.Units {
		if u.Status == "unknown" || u.Status == "running" {
			repairError(c, fmt.Errorf("存在未知执行结果，不允许自动重放"))
			return
		}
	}
	if e := validateRepairUnits(o.Units); e != nil {
		repairError(c, e)
		return
	}
	if e := validateRepairObjectScope(o, o.Units, j.DstDBType); e != nil {
		repairError(c, e)
		return
	}
	if e := validateMySQLPGTriggerDependencies(j.JobID, o); e != nil {
		repairError(c, e)
		return
	}
	_, dst, e := resolveRepairConnections(c, j, b, false)
	if e != nil {
		repairError(c, e)
		return
	}
	o.Verification = nil
	o.ManualVerification = nil
	a := &store.MigrationRepairAttempt{ID: uuid.NewString(), JobID: j.JobID, RequestID: req.RequestID, RequestHash: hash, ObjectID: o.ID, OwnerID: middleware.GetCurrentUserID(c), Kind: "ddl", Status: "running", Scope: migrationScope(dst, b.TargetSchema), SnapshotJSON: repairJSON(gin.H{"object": o, "binding": b}), UnitsJSON: repairJSON(o.Units)}
	claimed, newAttempt, e := store.ClaimRepairAttempt(a, row.Version, b.Version)
	if e != nil {
		repairError(c, e)
		return
	}
	if !newAttempt {
		c.JSON(202, attemptResponse(claimed))
		return
	}
	response := attemptResponse(a)
	startRepairWorker(a, func(ctx context.Context) {
		if e := validateMySQLPGTriggerDependencies(j.JobID, o); e != nil {
			finishRepair(a, o, e)
			return
		}
		w, e := buildDstWriter(dst, b.TargetSchema, target.ConnPoolConfig{})
		if e != nil {
			finishRepair(a, o, e)
			return
		}
		defer w.Close()
		if setter, ok := w.(interface{ SetSourceType(string) }); ok {
			setter.SetSourceType(j.SrcDBType)
		}
		units, e := target.ExecuteRepair(ctx, w, o, o.Units, func(units []repair.Unit) error {
			return persistRepairProgress(a, o, units, nil)
		})
		for i := range units {
			if units[i].Status == "succeeded" {
				units[i].Selected = false
			}
			if units[i].Status == "unknown" {
				a.Status = "unknown"
			}
		}
		a.UnitsJSON = repairJSON(units)
		finishRepair(a, o, e)
	})
	c.JSON(202, response)
}

// prepareMigrationRepair runs before the first mutation, fixing effective namespace and a shared target lock.
func prepareMigrationRepair(ctx context.Context, j *store.DataMigrationJob, src, dst *store.Connection, override string, cfg *datamigrate.Config, w target.Writer) (func(), error) {
	schema, e := target.ResolveRepairSchema(ctx, w)
	if e != nil {
		return nil, e
	}
	cfg.TargetSchema = schema
	cfg.CaptureRepair = true
	j.DstSchema = schema
	migrationSourceLocation(j, src, override)
	j.RepairConfigJSON = repairJSON(cfg)
	if e = store.DB.Model(&store.DataMigrationJob{}).Where("job_id = ?", j.JobID).Updates(map[string]any{"dst_schema": schema, "source_database": j.SourceDatabase, "source_connection_database": j.SourceConnectionDatabase, "source_schema": j.SourceSchema, "repair_config_json": j.RepairConfigJSON}).Error; e != nil {
		return nil, e
	}
	scope := migrationScope(dst, schema)
	if e = store.AcquireMigrationScope(scope, j.JobID); e != nil {
		return nil, e
	}
	cfg.RepairSink = func(m datamigrate.RepairManifest) error { return persistMigrationManifest(j, src, dst, m) }
	cfg.RepairObjectSink = func(o repair.Object) error { return persistMigrationRepairObject(j.JobID, o) }
	return func() {
		if e := store.ReleaseMigrationScope(scope, j.JobID); e != nil {
			slog.Error("release migration target lock", "job", j.JobID, "err", e)
		}
	}, nil
}

func persistRepairProgress(a *store.MigrationRepairAttempt, obj repair.Object, units []repair.Unit, previous []repair.Unit) error {
	obj.Units = units
	obj.Status = "running"
	a.UnitsJSON = repairJSON(append(append([]repair.Unit{}, previous...), units...))
	return store.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Model(&store.MigrationRepairAttempt{}).Where("id = ? AND status = ?", a.ID, "running").Update("units_json", a.UnitsJSON).Error; e != nil {
			return e
		}
		return tx.Model(&store.MigrationRepairObject{}).Where("job_id = ? AND object_id = ?", a.JobID, obj.ID).Update("object_json", repairJSON(obj)).Error
	})
}
