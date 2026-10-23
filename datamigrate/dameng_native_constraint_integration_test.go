package datamigrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbgold/datamigrate/damengnative"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
	"dbgold/datamigrate/validation"
	"github.com/stretchr/testify/require"
)

// The full schema path must retain a user-created index attached to a PK and
// enabled NOVALIDATE constraints with historical rows that violate the rule.
func TestDamengNativeConstraintStateIntegration(t *testing.T) {
	srcDSN, srcSchema := os.Getenv("DAMENG_NATIVE_SOURCE_DSN"), os.Getenv("DAMENG_NATIVE_SOURCE_SCHEMA")
	dstDSN, dstSchema := os.Getenv("DAMENG_NATIVE_TARGET_DSN"), os.Getenv("DAMENG_NATIVE_TARGET_SCHEMA")
	if os.Getenv("DAMENG_NATIVE_ALLOW_DDL") != "1" || srcDSN == "" || dstDSN == "" || srcSchema == "" || dstSchema == "" {
		t.Skip("需要独立预建的空 DM8 Schema、DAMENG_NATIVE_SOURCE_DSN/SOURCE_SCHEMA/TARGET_DSN/TARGET_SCHEMA 及 DAMENG_NATIVE_ALLOW_DDL=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	src, err := sql.Open("dm", srcDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	dst, err := sql.Open("dm", dstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })
	for _, d := range []struct {
		db *sql.DB
		s  string
	}{{src, srcSchema}, {dst, dstSchema}} {
		var count int
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYS.SYSOBJECTS o JOIN SYS.SYSOBJECTS s ON s.ID=o.SCHID WHERE s.NAME=?`, d.s).Scan(&count))
		require.Zero(t, count, "测试 Schema 必须初始完全为空")
	}
	prefix := fmt.Sprintf("Key%X", time.Now().UnixNano())
	parent, child, check, generated, unique := prefix+"Parent", prefix+"Child", prefix+"Check", prefix+"Generated", prefix+"Unique"
	idx, pk, ck, uk, fk := prefix+"Index", prefix+"PK", prefix+"CK", prefix+"UK", prefix+"FK"
	cluster, clusterIndex := prefix+"Cluster", prefix+"ClusterIndex"
	disabled, duplicateNamed := prefix+"Disabled", prefix+"DuplicateNamed"
	partitioned, partitionIndex := prefix+"Partitioned", prefix+"PartitionIndex"
	sq := func(n string) string { return damengnative.Qualified(srcSchema, n) }
	dq := func(n string) string { return damengnative.Qualified(dstSchema, n) }
	cleanup := func(db *sql.DB, qualify func(string) string) {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, name := range []string{child, partitioned, parent, check, generated, unique, cluster, disabled} {
			_, _ = db.ExecContext(clean, "DROP TABLE "+qualify(name)+" CASCADE CONSTRAINTS")
		}
	}
	t.Cleanup(func() { cleanup(dst, dq); cleanup(src, sq) })
	exec := func(q string) { _, e := src.ExecContext(ctx, q); require.NoError(t, e, q) }
	exec("CREATE TABLE " + sq(parent) + ` ("Id" INT, "Label" VARCHAR(40) DEFAULT 'USING INDEX keep')`)
	exec("CREATE INDEX " + sq(idx) + " ON " + sq(parent) + `("Id")`)
	exec("ALTER TABLE " + sq(parent) + " ADD CONSTRAINT " + damengnative.Quote(pk) + ` PRIMARY KEY("Id") USING INDEX ` + sq(idx))
	exec("INSERT INTO " + sq(parent) + `("Id") VALUES(1)`)
	exec("CREATE TABLE " + sq(child) + `("Pid" INT, CONSTRAINT ` + damengnative.Quote(fk) + ` FOREIGN KEY("Pid") REFERENCES ` + sq(parent) + `("Id"))`)
	exec("INSERT INTO " + sq(child) + ` VALUES(1)`)
	for _, name := range []string{check, generated} {
		exec("CREATE TABLE " + sq(name) + `("Value" INT)`)
		exec("INSERT INTO " + sq(name) + ` VALUES(-1)`)
	}
	exec("ALTER TABLE " + sq(check) + " ADD CONSTRAINT " + damengnative.Quote(ck) + ` CHECK("Value">0) ENABLE NOVALIDATE`)
	exec("ALTER TABLE " + sq(generated) + " ADD CONSTRAINT " + damengnative.Quote(duplicateNamed) + ` CHECK("Value">0) ENABLE NOVALIDATE`)
	exec("ALTER TABLE " + sq(generated) + ` ADD CHECK("Value">0) ENABLE NOVALIDATE`)
	exec("ALTER TABLE " + sq(generated) + ` ADD CHECK("Value">0) ENABLE NOVALIDATE`)
	exec("CREATE TABLE " + sq(disabled) + `("Id" INT)`)
	exec("INSERT INTO " + sq(disabled) + ` VALUES(-1)`)
	exec("ALTER TABLE " + sq(disabled) + ` ADD CHECK("Id">0) DISABLE`)
	exec("CREATE TABLE " + sq(unique) + `("Id" INT)`)
	exec("INSERT INTO " + sq(unique) + ` VALUES(1)`)
	exec("ALTER TABLE " + sq(unique) + " ADD CONSTRAINT " + damengnative.Quote(uk) + ` UNIQUE("Id") ENABLE NOVALIDATE`)
	exec("CREATE TABLE " + sq(cluster) + `("Id" INT,"Value" INT,"(" VARCHAR(20) DEFAULT ')')`)
	exec("CREATE CLUSTER INDEX " + sq(clusterIndex) + " ON " + sq(cluster) + `("Id" DESC)`)
	exec("INSERT INTO " + sq(cluster) + `("Id","Value") VALUES(1,10)`)
	exec("INSERT INTO " + sq(cluster) + `("Id","Value") VALUES(1,11)`)
	exec("CREATE TABLE " + sq(partitioned) + `("Id" INT,"A" INT) PARTITION BY RANGE("A")(PARTITION "Low" VALUES LESS THAN(10),PARTITION "High" VALUES LESS THAN(MAXVALUE))`)
	exec("CREATE UNIQUE INDEX " + sq(partitionIndex) + " ON " + sq(partitioned) + `("Id") GLOBAL`)
	exec("ALTER TABLE " + sq(partitioned) + " ADD CONSTRAINT " + damengnative.Quote(prefix+"PartitionPK") + ` PRIMARY KEY("Id") USING INDEX ` + sq(partitionIndex))
	exec("INSERT INTO " + sq(partitioned) + " VALUES(1,1)")
	exec("INSERT INTO " + sq(partitioned) + " VALUES(2,11)")
	exec("ALTER TABLE " + sq(partitioned) + " ADD CONSTRAINT " + damengnative.Quote(prefix+"PartitionCheck") + ` CHECK("A"<5) ENABLE NOVALIDATE`)
	exec("ALTER TABLE " + sq(partitioned) + " ADD CONSTRAINT " + damengnative.Quote(prefix+"PartitionFK") + ` FOREIGN KEY("Id") REFERENCES ` + sq(parent) + `("Id") ENABLE NOVALIDATE`)
	r, err := source.NewDaMengNative(srcDSN, srcSchema, source.ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	w, err := target.NewDaMengNative(dstDSN, dstSchema, target.ConnPoolConfig{})
	require.NoError(t, err)
	defer w.Close()
	var saved RepairManifest
	cfg := Config{Mode: "all", Content: "both", TargetDBType: "dameng", TargetSchema: dstSchema, CaptureRepair: true, ValidateData: true, PageSize: 1, MaxParallel: 2, RepairSink: func(p RepairManifest) error { saved = p; return nil }}
	m := NewMigrator(r, w, &Job{LogCh: make(chan string, 4096)}, cfg)
	preview := m.PreflightDamengNative(ctx)
	require.True(t, preview.Ready, "%+v", preview.Issues)
	var targetCount int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER=?`, dstSchema).Scan(&targetCount))
	require.Zero(t, targetCount, "预检必须保持目标零写入")
	report := m.Run(ctx)
	if report.HasFailures() {
		b, _ := json.Marshal(report)
		t.Logf("report=%s; logs=%v", b, drainLogs(m.job))
	}
	require.False(t, report.HasFailures())
	require.Equal(t, validation.StatusPassed, report.DataValidation.Status)
	var actualIndex string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT INDEX_NAME FROM ALL_CONSTRAINTS WHERE OWNER=? AND CONSTRAINT_NAME=?`, dstSchema, pk).Scan(&actualIndex))
	require.Equal(t, idx, actualIndex, "必须继续绑定原用户索引，不能换成系统生成的同列索引")
	var clusterOrder string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT DESCEND FROM ALL_IND_COLUMNS WHERE INDEX_OWNER=? AND INDEX_NAME=?`, dstSchema, clusterIndex).Scan(&clusterOrder))
	require.Equal(t, "DESC", clusterOrder)
	for _, name := range []string{prefix + "PartitionPK", prefix + "PartitionCheck", prefix + "PartitionFK"} {
		var status string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT STATUS FROM ALL_CONSTRAINTS WHERE OWNER=? AND CONSTRAINT_NAME=?`, dstSchema, name).Scan(&status))
		require.Equal(t, "ENABLED", status)
	}
	var physicalConstraints int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_CONSTRAINTS c JOIN SYS.SYSOBJECTS t ON t.NAME=c.TABLE_NAME JOIN SYS.SYSOBJECTS s ON s.ID=t.SCHID AND s.NAME=c.OWNER JOIN SYS.SYSHPARTTABLEINFO p ON p.PART_TABLE_ID=t.ID JOIN SYS.SYSOBJECTS b ON b.ID=p.BASE_TABLE_ID WHERE c.OWNER=? AND b.NAME=?`, dstSchema, partitioned).Scan(&physicalConstraints))
	require.Equal(t, 6, physicalConstraints, "每个实际物理分区必须恢复根PK/CHECK/FK的三个派生约束")
	var duplicateCount int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=?`, dstSchema, generated).Scan(&duplicateCount))
	require.Equal(t, 3, duplicateCount, "同表达式的一个命名约束和两个生成约束必须全部保留")
	var disabledState string
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT STATUS FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=?`, dstSchema, disabled).Scan(&disabledState))
	require.Equal(t, "DISABLED", disabledState)
	_, err = dst.ExecContext(ctx, "INSERT INTO "+dq(disabled)+" VALUES(-2)")
	require.NoError(t, err, "生成名称的禁用约束必须从创建起保持禁用")
	_, err = dst.ExecContext(ctx, "DELETE FROM "+dq(disabled)+" WHERE "+damengnative.Quote("Id")+"=-2")
	require.NoError(t, err)
	for _, name := range []string{check, generated, unique} {
		var status, validated string
		require.NoError(t, dst.QueryRowContext(ctx, `SELECT STATUS,VALIDATED FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=?`, dstSchema, name).Scan(&status, &validated))
		require.Equal(t, "ENABLED", status)
		require.Equal(t, "NOT VALIDATED", validated)
	}
	for _, name := range []string{check, generated} {
		var value int
		require.NoError(t, dst.QueryRowContext(ctx, "SELECT \"Value\" FROM "+dq(name)).Scan(&value))
		require.Equal(t, -1, value)
		_, err := dst.ExecContext(ctx, "INSERT INTO "+dq(name)+" VALUES(-2)")
		require.Error(t, err, "历史不满足约束的行保留，后续写入仍需受约束保护")
	}
	_, err = dst.ExecContext(ctx, "INSERT INTO "+dq(parent)+`("Id") VALUES(1)`)
	require.Error(t, err, "原索引继续执行主键唯一性语义")
	serialized, err := json.Marshal(saved)
	require.NoError(t, err)
	var reload RepairManifest
	require.NoError(t, json.Unmarshal(serialized, &reload))
	proofs := 0
	for _, object := range reload.Objects {
		if object.Native == nil || object.Native.Kind != "table" {
			continue
		}
		for i, unit := range object.Native.Units {
			upper := strings.ToUpper(unit.SQL)
			if !strings.Contains(upper, "CREATE OR REPLACE UNIQUE") && !strings.Contains(upper, "CREATE OR REPLACE CLUSTER") && !(strings.HasPrefix(upper, "ALTER TABLE") && strings.Contains(upper, " ADD ")) {
				continue
			}
			unknown := object
			unknown.Units = append(unknown.Units[:0:0], object.Units...)
			unknown.Units[i].Status, unknown.Units[i].Selected = "unknown", true
			reconciled, err := target.ExecuteRepair(ctx, w, unknown, unknown.Units, nil)
			require.NoError(t, err, "已完成的子对象必须先核对其实际定义，不能重放 CREATE/ADD: %s", unit.SQL)
			require.Equal(t, "succeeded", reconciled[i].Status)
			require.False(t, reconciled[i].Selected)
			proofs++
		}
	}
	require.GreaterOrEqual(t, proofs, 9)
	// Simulate a known missing second generated ADD after a lost result.
	// The named same-body CHECK must not be counted as that generated unit.
	actual, err := damengnative.ReadCatalogForVerification(ctx, dst, dstSchema)
	require.NoError(t, err)
	var generatedNames []string
	for _, object := range actual.Objects {
		if object.Kind == "constraint" && object.Table == generated && object.Generated {
			generatedNames = append(generatedNames, object.Name)
		}
	}
	require.Len(t, generatedNames, 2)
	_, err = dst.ExecContext(ctx, "ALTER TABLE "+dq(generated)+" DROP CONSTRAINT "+damengnative.Quote(generatedNames[1]))
	require.NoError(t, err)
	issues, err := w.VerifyDamengNative(ctx, preview.Snapshot)
	require.NoError(t, err)
	require.True(t, damengnative.HasErrors(issues), "整体验证必须识别缺失的第二个无名约束，不能复用另一同表达式定义: %+v", issues)
	for _, object := range reload.Objects {
		if object.Native == nil || object.Native.Kind != "table" || object.Native.Name != generated {
			continue
		}
		missing := object
		missing.Units = append(missing.Units[:0:0], object.Units...)
		last := -1
		for i, unit := range missing.Native.Units {
			if strings.Contains(unit.SQL, " ADD ") && !strings.Contains(unit.SQL, " ADD CONSTRAINT ") {
				last = i
			}
		}
		require.NotEqual(t, -1, last)
		missing.Units[last].Status, missing.Units[last].Selected = "unknown", true
		reconciled, err := target.ExecuteRepair(ctx, w, missing, missing.Units, nil)
		require.NoError(t, err)
		require.Equal(t, "succeeded", reconciled[last].Status)
	}
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_CONSTRAINTS WHERE OWNER=? AND TABLE_NAME=?`, dstSchema, generated).Scan(&duplicateCount))
	require.Equal(t, 3, duplicateCount, "补迁必须真实创建缺失的第二个无名约束")
	require.NoError(t, VerifyNativeRepairFinalization(ctx, r, w, cfg, reload.Objects))
}
