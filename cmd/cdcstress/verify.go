package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Verification struct {
	StartedAt      time.Time           `json:"started_at"`
	FinishedAt     time.Time           `json:"finished_at"`
	Match          bool                `json:"match"`
	Tables         []TableVerification `json:"tables"`
	Mismatches     int                 `json:"mismatches"`
	ScopeIsolation bool                `json:"scope_isolation"`
}

type TableVerification struct {
	Table        string `json:"table"`
	Strategy     string `json:"strategy"`
	SourceCount  int64  `json:"source_count"`
	TargetCount  int64  `json:"target_count"`
	SourceDigest string `json:"source_digest"`
	TargetDigest string `json:"target_digest"`
	Match        bool   `json:"match"`
	Error        string `json:"error,omitempty"`
}

type rowDigest struct {
	count int64
	sha   hash.Hash
	sum   [4]uint64
	xor   [4]uint64
}

func verifyAndReport(ctx context.Context, cfg Config, state *RunState) (Verification, error) {
	dbs, err := openDatabases(ctx, cfg, true)
	if err != nil {
		return Verification{}, err
	}
	defer dbs.close()
	verification, err := verify(ctx, cfg, state, dbs)
	if err == nil && (state.FailureClass != "" || state.ActiveScenario != "") {
		err = fmt.Errorf("run has unresolved scenario state: active=%s failed=%s class=%s", state.ActiveScenario, state.FailedScenario, state.FailureClass)
	}
	report := RunReport{RunID: state.RunID, StartedAt: verification.StartedAt, FinishedAt: verification.FinishedAt, ConfigHash: cfg.hash(), TableCount: len(state.Tables), TotalRows: cfg.Profile.TotalRows,
		Environment: collectEnvironment(ctx, cfg, dbs), Verification: verification, Passed: err == nil && verification.Match,
		Workloads: append([]WorkloadResult(nil), state.Workloads...), FailureClass: state.FailureClass, FailedScenario: state.FailedScenario,
		RecoveryAttempts: state.RecoveryAttempts, SkippedLegacy: append([]string(nil), state.SkippedLegacy...)}
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	if writeErr := writeRunReport(cfg, report); writeErr != nil && err == nil {
		err = writeErr
	}
	return verification, err
}

func verify(ctx context.Context, cfg Config, state *RunState, dbs *databases) (Verification, error) {
	result := Verification{StartedAt: time.Now().UTC(), Match: true, ScopeIsolation: true, Tables: make([]TableVerification, 0, len(state.Tables))}
	var noiseCount int
	if err := dbs.gauss.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN ('cdcstress_noise_outside_scope','cdcstress_cross_database_noise')", cfg.GaussDB.Schema).Scan(&noiseCount); err != nil || noiseCount != 0 {
		result.Match, result.ScopeIsolation = false, false
		result.Mismatches++
	}
	for i, table := range state.Tables {
		item := TableVerification{Table: table.Name, Strategy: string(table.Kind)}
		sourceDigest, sourceCount, err := digestTable(ctx, dbs.mysql, cfg, table, false)
		if err != nil {
			item.Error = "source: " + err.Error()
		}
		targetDigest, targetCount, targetErr := digestTable(ctx, dbs.gauss, cfg, table, true)
		if targetErr != nil {
			if item.Error != "" {
				item.Error += "; "
			}
			item.Error += "target: " + targetErr.Error()
		}
		item.SourceCount, item.TargetCount = sourceCount, targetCount
		item.SourceDigest, item.TargetDigest = sourceDigest, targetDigest
		item.Match = item.Error == "" && sourceCount == targetCount && sourceDigest == targetDigest
		if !item.Match {
			result.Match = false
			result.Mismatches++
		}
		result.Tables = append(result.Tables, item)
		if (i+1)%100 == 0 {
			fmt.Printf("verified %d/%d tables\n", i+1, len(state.Tables))
		}
	}
	result.FinishedAt = time.Now().UTC()
	if !result.Match {
		return result, fmt.Errorf("%d of %d tables differ", result.Mismatches, len(result.Tables))
	}
	return result, nil
}

func digestTable(ctx context.Context, db *sql.DB, cfg Config, table TableSpec, target bool) (string, int64, error) {
	columns := commonColumnNames(table)
	if scenarioColumnExists(ctx, db, cfg, table, target, "cdc_extra") {
		columns = append(columns, "cdc_extra")
	}
	quote := mysqlIdent
	qualified := mysqlIdent(table.Name)
	if target {
		quote = pgIdent
		name := table.Name
		if cfg.Profile.LowerCaseNames {
			name = strings.ToLower(name)
		}
		qualified = pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(name)
	}
	query := "SELECT " + strings.Join(quoteColumns(columns, quote), ",") + " FROM " + qualified
	ordered := table.Kind != kindKeyless
	if ordered {
		switch table.Kind {
		case kindComposite:
			query += " ORDER BY " + quote("tenant_id") + "," + quote("id")
		case kindUnique:
			query += " ORDER BY " + quote("code")
		default:
			query += " ORDER BY " + quote("id")
		}
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	digest := rowDigest{sha: sha256.New()}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return "", digest.count, err
		}
		rowHash := canonicalRowHash(columns, values)
		digest.count++
		if ordered {
			_, _ = digest.sha.Write(rowHash[:])
		} else {
			for i := 0; i < 4; i++ {
				value := binary.BigEndian.Uint64(rowHash[i*8 : (i+1)*8])
				digest.sum[i] += value
				digest.xor[i] ^= value
			}
		}
	}
	if err = rows.Err(); err != nil {
		return "", digest.count, err
	}
	if ordered {
		return hex.EncodeToString(digest.sha.Sum(nil)), digest.count, nil
	}
	buffer := make([]byte, 72)
	binary.BigEndian.PutUint64(buffer[:8], uint64(digest.count))
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint64(buffer[8+i*8:16+i*8], digest.sum[i])
		binary.BigEndian.PutUint64(buffer[40+i*8:48+i*8], digest.xor[i])
	}
	sum := sha256.Sum256(buffer)
	return hex.EncodeToString(sum[:]), digest.count, nil
}

func canonicalRowHash(columns []string, values []any) [32]byte {
	h := sha256.New()
	for i, value := range values {
		text := canonicalValue(columns[i], value)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(text)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(text))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func canonicalValue(column string, value any) string {
	if value == nil {
		return "<NULL>"
	}
	if t, ok := value.(time.Time); ok {
		return t.UTC().Format("2006-01-02T15:04:05.000000")
	}
	if b, ok := value.([]byte); ok {
		if column == "blob_data" {
			return "hex:" + bytesHex(b)
		}
		return normalizeText(column, string(b))
	}
	return normalizeText(column, fmt.Sprint(value))
}

func normalizeText(column, value string) string {
	switch column {
	case "amount":
		if strings.Contains(value, ".") {
			value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
		}
		if value == "-0" || value == "" {
			return "0"
		}
	case "active":
		if strings.EqualFold(value, "true") {
			return "1"
		}
		if strings.EqualFold(value, "false") {
			return "0"
		}
	case "created_at":
		for _, layout := range []string{"2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05.999999999", time.RFC3339Nano} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC().Format("2006-01-02T15:04:05.000000")
			}
		}
	}
	return value
}

func scenarioColumnExists(ctx context.Context, db *sql.DB, cfg Config, table TableSpec, target bool, column string) bool {
	var count int
	if target {
		name := table.Name
		if cfg.Profile.LowerCaseNames {
			name = strings.ToLower(name)
		}
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3", cfg.GaussDB.Schema, name, column).Scan(&count)
		return err == nil && count == 1
	}
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=? AND table_name=? AND column_name=?", cfg.MySQL.Database, table.Name, column).Scan(&count)
	return err == nil && count == 1
}

func writeRunReport(cfg Config, report RunReport) error {
	dir := cfg.resultDir(report.RunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(renderMarkdownReport(report)), 0o600)
}

func renderMarkdownReport(report RunReport) string {
	var md strings.Builder
	fmt.Fprintf(&md, "# CDC 压力测试报告 `%s`\n\n", report.RunID)
	fmt.Fprintf(&md, "- 测试结果：**%s**\n- 表数量：%d\n- 初始总行数：%d\n- 最大观测延迟：%d 秒\n- 开始时间：%s\n- 结束时间：%s\n\n",
		chineseResult(report.Passed), report.TableCount, report.TotalRows, report.MaxLagSecs, formatReportTime(report.StartedAt), formatReportTime(report.FinishedAt))
	fmt.Fprintf(&md, "## 环境与资源峰值\n\n- 运行环境：%s %s/%s\n- MySQL：%s（%s）\n- GaussDB：%s（%s）\n- MySQL 线程峰值：连接线程=%d，运行线程=%d\n- GaussDB 会话峰值：%d\n\n",
		report.Environment.GoVersion, report.Environment.OS, report.Environment.Architecture, report.Environment.MySQLVersion, report.Environment.Source,
		report.Environment.GaussDBVersion, report.Environment.Target, report.ResourcePeaks.MySQLThreadsConnected, report.ResourcePeaks.MySQLThreadsRunning, report.ResourcePeaks.GaussDBSessions)
	if len(report.Workloads) > 0 {
		md.WriteString("## 负载性能基线\n\n| 测试阶段 | 目标 TPS | 实际 TPS | 提交数 | 错误数 | 错误分类 | P50（毫秒） | P95（毫秒） | P99（毫秒） |\n|---|---:|---:|---:|---:|---|---:|---:|---:|\n")
		for _, workload := range report.Workloads {
			p50, p95, p99 := percentiles(workload.LatencyMS)
			fmt.Fprintf(&md, "| %s | %d | %.1f | %d | %d | %s | %.1f | %.1f | %.1f |\n", chineseStageName(workload.Name), workload.TargetTPS, workload.ActualTPS, workload.Committed, workload.Errors, chineseFailureClass(workload.FailureClass), p50, p95, p99)
		}
		md.WriteString("\n")
		var hasErrors bool
		for _, workload := range report.Workloads {
			if len(workload.ErrorSamples) > 0 {
				hasErrors = true
			}
		}
		if hasErrors {
			md.WriteString("### 负载错误样例\n\n")
			for _, workload := range report.Workloads {
				for _, sample := range workload.ErrorSamples {
					fmt.Fprintf(&md, "- `%s`：%s\n", chineseStageName(workload.Name), sample)
				}
			}
			md.WriteString("\n")
		}
	}
	if report.RecoveryAttempts > 0 || report.FailedScenario != "" || len(report.SkippedLegacy) > 0 {
		md.WriteString("## 断点与恢复\n\n")
		fmt.Fprintf(&md, "- 恢复请求次数：%d\n", report.RecoveryAttempts)
		if report.FailedScenario != "" {
			fmt.Fprintf(&md, "- 失败阶段：%s\n- 错误分类：%s\n", chineseStageName(report.FailedScenario), chineseFailureClass(report.FailureClass))
		}
		if len(report.SkippedLegacy) > 0 {
			md.WriteString("- 旧版本运行中已跳过且没有持久化性能指标的阶段：\n")
			for _, stage := range report.SkippedLegacy {
				fmt.Fprintf(&md, "  - %s\n", chineseStageName(stage))
			}
		}
		md.WriteString("\n")
	}
	fmt.Fprintf(&md, "## 数据校验\n\n- 数据一致：%s\n- 迁移范围隔离正确：%s\n- 不一致检查项：%d\n\n", chineseBool(report.Verification.Match), chineseBool(report.Verification.ScopeIsolation), report.Verification.Mismatches)
	if len(report.Errors) > 0 {
		md.WriteString("## 运行错误\n\n")
		for _, item := range report.Errors {
			fmt.Fprintf(&md, "- %s\n", item)
		}
	}
	return md.String()
}

func chineseFailureClass(class string) string {
	switch class {
	case failureEnvironment:
		return "压测环境或连接故障"
	case failureCapacity:
		return "数据库容量边界"
	case failureCorrectness:
		return "正确性错误"
	case "":
		return "无"
	default:
		return class
	}
}

func chineseResult(passed bool) string {
	if passed {
		return "通过"
	}
	return "失败"
}

func chineseBool(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

func chineseStageName(name string) string {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 {
		return name
	}
	mode := map[string]string{"full_then_cdc": "全量快照后持续同步", "incremental_only": "指定位点增量同步"}[parts[0]]
	if mode == "" {
		mode = parts[0]
	}
	stage := parts[1]
	switch stage {
	case "snapshot-concurrent-write":
		stage = "全量快照期间并发写入"
	case "burst":
		stage = "突发负载"
	case "writes-while-paused":
		stage = "任务暂停期间写入"
	case "after-binlog-rotate":
		stage = "binlog 轮换后写入"
	case "transaction-boundaries":
		stage = "事务边界"
	case "pause-resume":
		stage = "暂停与恢复"
	case "binlog-rotate":
		stage = "binlog 轮换与空闲恢复"
	case "target-conflict":
		stage = "目标端冲突修复"
	case "application-restart":
		stage = "dbgold 应用重启恢复"
	case "cutover":
		stage = "切换收口"
	default:
		if strings.HasPrefix(stage, "steady-") && strings.HasSuffix(stage, "tps") {
			value := strings.TrimSuffix(strings.TrimPrefix(stage, "steady-"), "tps")
			stage = "稳态负载（" + value + " TPS）"
		}
	}
	return mode + " / " + stage
}

func formatReportTime(value time.Time) string {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.FixedZone("CST", 8*60*60)
	}
	return value.In(location).Format("2006年01月02日 15:04:05 MST")
}

func parseInt(value any) int64 { n, _ := strconv.ParseInt(fmt.Sprint(value), 10, 64); return n }
