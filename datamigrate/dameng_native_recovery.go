package datamigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

type nativeRecoveryState interface {
	NativeRecoveryState(context.Context, string) ([]damengnative.Unit, []damengnative.Unit, error)
}

func previewDamengNativeRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, obj repair.Object, related []repair.Object, mapping map[string]string) (repair.RecoveryPreview, error) {
	p := repair.RecoveryPreview{ObjectID: obj.ID, SourceTable: obj.SourceTable, TargetTable: obj.Table, Warnings: []string{"接续使用当前源数据，源表和目标表必须停写；非空目标必须确认清空后完整重导。"}, Related: []repair.Object{}, ColumnMapping: map[string]string{}}
	if obj.Kind != "table" && obj.Kind != "data" {
		return p, fmt.Errorf("原生数据接续仅支持表/数据对象")
	}
	if obj.NativeTable == nil {
		return p, fmt.Errorf("缺少冻结的达梦原生表计划，不能使用通用数据通路补迁")
	}
	if !obj.TrustedNative() {
		return p, fmt.Errorf("缺少可信原生表执行计划，不能使用通用数据通路补迁")
	}
	p.NativeObject = obj.Native
	p.NativeUnits = append([]repair.Unit{}, obj.Units...)
	if err := validateNativeRecoveryFoundations(p.NativeObject, p.NativeUnits); err != nil {
		return p, err
	}
	nr, ok := r.(damengnative.Reader)
	if !ok {
		return p, fmt.Errorf("原生 Reader 不可用")
	}
	nw, ok := w.(damengnative.Writer)
	if !ok {
		return p, fmt.Errorf("原生 Writer 不可用")
	}
	state, ok := w.(nativeRecoveryState)
	if !ok {
		return p, fmt.Errorf("目标缺少原生数据接续状态检查能力")
	}
	src, err := nr.SnapshotDamengNative(ctx)
	if err != nil {
		return p, err
	}
	if damengnative.HasErrors(src.Issues) {
		return p, fmt.Errorf("源完整对象预检失败，请修复后再补迁")
	}
	dst, err := nw.InspectDamengNative(ctx)
	if err != nil {
		return p, err
	}
	if damengnative.HasErrors(dst.Issues) {
		return p, fmt.Errorf("目标原生目录/事件触发器预检失败，禁止数据补迁")
	}
	for _, o := range dst.Objects {
		if o.Kind == "trigger" && !o.Managed && o.EventTrigger && (strings.EqualFold(o.Status, "ENABLED") || o.Status == "Y" || o.Status == "1") {
			return p, fmt.Errorf("目标存在启用的事件触发器，禁止补迁期间触发事件副作用")
		}
	}
	if nativeSameDatabase(src.Profile, dst.Profile) && src.Profile.Schema == dst.Profile.Schema {
		return p, fmt.Errorf("源目标不能是同一实例同一 Schema")
	}
	if cfg.TargetSchema != "" && dst.Profile.Schema != cfg.TargetSchema {
		return p, fmt.Errorf("目标 Schema 已改变")
	}
	if src.Profile.Version == "" || src.Profile.Version != dst.Profile.Version {
		return p, fmt.Errorf("补迁源目标 DM8 版本不一致")
	}
	for key, value := range src.Profile.Settings {
		if targetValue, ok := dst.Profile.Settings[key]; !ok || targetValue != value {
			return p, fmt.Errorf("补迁源目标语义参数不同: %s", key)
		}
	}
	if p.SourceTable == "" {
		p.SourceTable = nativeSourceTable(*obj.NativeTable)
	}
	if p.TargetTable == "" {
		p.TargetTable = obj.NativeTable.Name
	}
	var table *damengnative.Table
	for _, t := range src.Tables {
		if t.ObjectID == obj.NativeTable.ObjectID && nativeSourceTable(t) == p.SourceTable {
			copy := t
			table = &copy
			break
		}
	}
	if table == nil {
		return p, fmt.Errorf("源表身份已改变或不存在")
	}
	if damengnative.Fingerprint(table.Columns) != damengnative.Fingerprint(obj.NativeTable.Columns) {
		return p, fmt.Errorf("源列定义偏离冻结原生计划")
	}
	if table.CaptureRowID != obj.NativeTable.CaptureRowID || table.RowIDMapping != obj.NativeTable.RowIDMapping {
		return p, fmt.Errorf("源物理 ROWID 映射偏离冻结原生计划")
	}
	var targetTable *damengnative.Table
	for _, t := range dst.Tables {
		if t.Name == p.TargetTable {
			copy := t
			targetTable = &copy
			break
		}
	}
	if targetTable == nil {
		return p, fmt.Errorf("目标表不存在，先恢复原生表结构")
	}
	if len(table.Columns) != len(targetTable.Columns) {
		return p, fmt.Errorf("源目标原生列数量不同")
	}
	for i, c := range table.Columns {
		d := targetTable.Columns[i]
		mapped, _, e := damengnative.RewriteSchema(c.SQLType, table.Schema, dst.Profile.Schema)
		if e != nil {
			return p, e
		}
		if c.Name != d.Name || c.Writable != d.Writable || !strings.EqualFold(strings.TrimSpace(mapped), strings.TrimSpace(d.SQLType)) {
			return p, fmt.Errorf("源目标原生列定义不一致: %s", c.Name)
		}
		if targetName, ok := mapping[c.Name]; ok && targetName != c.Name {
			return p, fmt.Errorf("原生补迁不允许改写列名")
		}
		p.ColumnMapping[c.Name] = c.Name
		p.SourceColumns = append(p.SourceColumns, source.ColumnInfo{Name: c.Name, DataType: c.DataType, FormattedType: c.SQLType, Precision: c.Precision, Scale: c.Scale})
		p.TargetColumns = append(p.TargetColumns, source.ColumnInfo{Name: d.Name, DataType: d.DataType, FormattedType: d.SQLType, Precision: d.Precision, Scale: d.Scale})
	}
	p.NativeTable = table
	p.QualifiedTarget = damengnative.Qualified(dst.Profile.Schema, p.TargetTable)
	p.SourceRows = table.Rows
	p.TargetRows, err = w.CountRows(ctx, p.TargetTable)
	if err != nil {
		return p, err
	}
	p.NativeDisable, p.NativeRestore, err = state.NativeRecoveryState(ctx, p.TargetTable)
	if err != nil {
		return p, err
	}
	for _, o := range related {
		if o.Status == "succeeded" || o.Native == nil || o.Native.Managed {
			continue
		}
		if !o.TrustedNative() {
			return p, fmt.Errorf("依赖对象不具备可信原生计划: %s", o.Name)
		}
		p.Related = append(p.Related, o)
	}
	p.DataRequired = obj.DataStatus != "succeeded"
	if !p.DataRequired && p.SourceRows != p.TargetRows {
		return p, fmt.Errorf("已完成数据的行数发生变化，请重新核对")
	}
	p.Fingerprint = recoveryFingerprint(cfg, p)
	return p, nil
}

func runDamengNativeRecovery(ctx context.Context, r source.Reader, w target.Writer, cfg Config, p repair.RecoveryPreview, truncate bool, progress func(repair.RecoveryResult) error) (res repair.RecoveryResult, err error) {
	res = repair.RecoveryResult{Status: "running", DataStatus: "running", SourceRows: p.SourceRows, TargetRows: p.TargetRows}
	finish := func(e error) (repair.RecoveryResult, error) {
		res.Status = "succeeded"
		if e != nil {
			res.Status = "failed"
			res.Error = e.Error()
			if ctx.Err() != nil {
				res.Status = "unknown"
			}
		}
		if progress != nil {
			if pe := progress(res); pe != nil {
				res.Status = "unknown"
				return res, errors.Join(e, pe)
			}
		}
		return res, e
	}
	if p.NativeTable == nil || p.Fingerprint != recoveryFingerprint(cfg, p) {
		return finish(fmt.Errorf("原生接续预览签名无效"))
	}
	obj := repair.Object{ID: p.ObjectID, Kind: "table", SourceTable: p.SourceTable, Table: p.TargetTable, NativeTable: p.NativeTable, Native: p.NativeObject, Units: p.NativeUnits}
	if obj.Native != nil {
		obj.Schema, obj.TargetName = obj.Native.Schema, obj.Native.Name
		obj.NativeFingerprint = damengnative.Fingerprint(obj.Native)
	}
	if !p.DataRequired {
		obj.DataStatus = "succeeded"
	}
	fresh, e := previewDamengNativeRecovery(ctx, r, w, cfg, obj, p.Related, p.ColumnMapping)
	if e != nil {
		return finish(e)
	}
	if fresh.Fingerprint != p.Fingerprint {
		return finish(fmt.Errorf("源目标定义、行数或依赖状态已变化，请重新预览"))
	}
	if !p.DataRequired {
		res.DataStatus = "succeeded"
		return finish(executePendingNativeTable(ctx, w.(damengnative.Writer), p, nil))
	}
	if p.TargetRows > 0 && !truncate {
		return finish(fmt.Errorf("目标表非空，必须单独确认清空本表后完整重导"))
	}
	nr := r.(damengnative.Reader)
	nw := w.(damengnative.Writer)
	// Open the source before disabling constraints or clearing target data.
	scan, e := nr.OpenDamengNativeScan(ctx, *p.NativeTable)
	if e != nil {
		return finish(e)
	}
	_ = scan.Close()
	if progress != nil {
		if e = progress(res); e != nil {
			return finish(e)
		}
	}
	var restored bool
	defer func() {
		if restored {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var cleanupErr error
		for _, u := range p.NativeRestore {
			cleanupErr = errors.Join(cleanupErr, nw.ExecuteDamengNative(cleanup, u.SQL))
		}
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("恢复原始依赖状态失败: %w", cleanupErr))
			res.Status = "failed"
			res.Error = err.Error()
			if progress != nil {
				_ = progress(res)
			}
		}
	}()
	for _, u := range p.NativeDisable {
		if e = nw.ExecuteDamengNative(ctx, u.SQL); e != nil {
			return finish(e)
		}
	}
	if truncate {
		if e = nw.ExecuteDamengNative(ctx, "TRUNCATE TABLE "+p.QualifiedTarget); e != nil {
			return finish(e)
		}
		res.TargetRows = 0
	}
	if p.NativeTable.CaptureRowID && res.TargetRows == 0 {
		// This map is owned by the frozen table import. A previous batch stores
		// rows and mappings atomically; whole-table recovery clears both before
		// acquiring new target ROWIDs, never appending to an old physical map.
		if p.NativeTable.RowIDMapping == "" || p.NativeTable.RowIDMapping == p.TargetTable {
			return finish(fmt.Errorf("冻结物理 ROWID 映射表无效"))
		}
		if e = nw.ExecuteDamengNative(ctx, "TRUNCATE TABLE "+damengnative.Qualified(cfg.TargetSchema, p.NativeTable.RowIDMapping)); e != nil {
			return finish(e)
		}
	}
	n, e := copyDamengNativeTable(ctx, nr, nw, *p.NativeTable, cfg.TargetSchema, cfg.PageSize, func(n int64) error {
		res.CommittedRows = n
		if progress != nil {
			return progress(res)
		}
		return nil
	})
	res.CommittedRows = n
	if e != nil {
		return finish(fmt.Errorf("原生写入失败，禁止追加重试: %w", e))
	}
	for _, u := range p.NativeTable.Restore {
		sql, _, rewriteErr := damengnative.RewriteSchema(u.SQL, p.NativeTable.Schema, cfg.TargetSchema)
		if rewriteErr != nil {
			return finish(rewriteErr)
		}
		if e = nw.ExecuteDamengNative(ctx, sql); e != nil {
			return finish(e)
		}
	}
	completedRestores := map[string]bool{}
	for _, u := range p.NativeTable.Restore {
		sql, _, _ := damengnative.RewriteSchema(u.SQL, p.NativeTable.Schema, cfg.TargetSchema)
		completedRestores[sql] = true
	}
	if e = executePendingNativeTable(ctx, nw, p, completedRestores); e != nil {
		return finish(e)
	}
	res.TargetRows, e = w.CountRows(ctx, p.TargetTable)
	if e != nil {
		return finish(e)
	}
	if n != p.SourceRows || res.TargetRows != p.SourceRows {
		return finish(fmt.Errorf("原生补迁行数不一致: 源=%d 已提交=%d 目标=%d", p.SourceRows, n, res.TargetRows))
	}
	for _, u := range p.NativeRestore {
		if e = nw.ExecuteDamengNative(ctx, u.SQL); e != nil {
			return finish(e)
		}
	}
	restored = true
	res.DataStatus = "succeeded"
	return finish(nil)
}

func executePendingNativeTable(ctx context.Context, w damengnative.Writer, p repair.RecoveryPreview, alreadyExecuted map[string]bool) error {
	if err := validateNativeRecoveryFoundations(p.NativeObject, p.NativeUnits); err != nil {
		return err
	}
	for _, u := range p.NativeUnits {
		if u.Status == "succeeded" || alreadyExecuted[u.SQL] {
			continue
		}
		if err := w.ExecuteDamengNative(ctx, u.SQL); err != nil {
			return err
		}
	}
	return nil
}

// Foundational units belong to the frozen DDL repair entry point. In
// particular, interval_prepare is a typed operation rather than executable
// SQL, and must finish before a recovery can clear or populate the base table.
func validateNativeRecoveryFoundations(object *damengnative.Object, units []repair.Unit) error {
	if object == nil || len(object.Units) != len(units) {
		return fmt.Errorf("原生表接续缺少冻结执行单元")
	}
	for i, unit := range units {
		switch object.Units[i].Phase {
		case "before_data", "drop", "interval_prepare":
			if unit.Status != "succeeded" {
				return fmt.Errorf("表基础步骤 %s 尚未确认完成，请先通过冻结 DDL 修复入口完成或核验结构补迁", object.Units[i].Phase)
			}
		}
		if unit.Status == "unknown" || unit.Status == "running" {
			return fmt.Errorf("原生表包含执行结果未知的 DDL，请先核验后再恢复数据")
		}
	}
	return nil
}
