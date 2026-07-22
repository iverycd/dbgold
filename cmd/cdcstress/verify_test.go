package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCanonicalValueNormalizesDriverRepresentations(t *testing.T) {
	if got := canonicalValue("amount", []byte("12.3400")); got != "12.34" {
		t.Fatalf("amount=%q", got)
	}
	if got := canonicalValue("active", true); got != "1" {
		t.Fatalf("active=%q", got)
	}
	wantTime := "2026-07-22T10:11:12.123456"
	stamp := time.Date(2026, 7, 22, 10, 11, 12, 123456000, time.UTC)
	if got := canonicalValue("created_at", stamp); got != wantTime {
		t.Fatalf("time=%q", got)
	}
	if got := canonicalValue("blob_data", []byte{0, 1, 255}); got != "hex:0001ff" {
		t.Fatalf("blob=%q", got)
	}
}

func TestCanonicalRowHashIncludesNullAndBoundaries(t *testing.T) {
	a := canonicalRowHash([]string{"payload", "note"}, []any{"ab", "c"})
	b := canonicalRowHash([]string{"payload", "note"}, []any{"a", "bc"})
	c := canonicalRowHash([]string{"payload", "note"}, []any{"ab", nil})
	if bytes.Equal(a[:], b[:]) || bytes.Equal(a[:], c[:]) {
		t.Fatal("row encoding is ambiguous")
	}
}

func TestPercentiles(t *testing.T) {
	p50, p95, p99 := percentiles([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if p50 != 5 || p95 != 9 || p99 != 9 {
		t.Fatalf("percentiles=%v/%v/%v", p50, p95, p99)
	}
}

func TestConflictRepairRoundTrip(t *testing.T) {
	stamp := time.Date(2026, 7, 22, 10, 11, 12, 123456000, time.UTC)
	repair := encodeConflictRepair("cs_table", []string{"id", "blob_data", "created_at", "note"}, []any{"42", []byte{0, 1, 255}, stamp, nil})
	values, err := decodeConflictRepair(repair)
	if err != nil {
		t.Fatal(err)
	}
	if values[0] != "42" || !bytes.Equal(values[1].([]byte), []byte{0, 1, 255}) || !values[2].(time.Time).Equal(stamp) || values[3] != nil {
		t.Fatalf("unexpected decoded values: %#v", values)
	}
}

func TestChineseStageName(t *testing.T) {
	tests := map[string]string{
		"full_then_cdc/snapshot-concurrent-write": "全量快照后持续同步 / 全量快照期间并发写入",
		"full_then_cdc/steady-1000tps":            "全量快照后持续同步 / 稳态负载（1000 TPS）",
		"full_then_cdc/burst":                     "全量快照后持续同步 / 突发负载",
		"incremental_only/writes-while-paused":    "指定位点增量同步 / 任务暂停期间写入",
		"incremental_only/after-binlog-rotate":    "指定位点增量同步 / binlog 轮换后写入",
	}
	for input, want := range tests {
		if got := chineseStageName(input); got != want {
			t.Errorf("%q => %q, want %q", input, got, want)
		}
	}
}

func TestRenderMarkdownReportUsesChinesePresentation(t *testing.T) {
	started := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	report := RunReport{
		RunID: "run_test", StartedAt: started, FinishedAt: started.Add(time.Minute), Passed: false,
		TableCount: 100, TotalRows: 1_000_000, MaxLagSecs: 12,
		Environment:   EnvironmentSummary{GoVersion: "go1.25.5", OS: "darwin", Architecture: "arm64", MySQLVersion: "5.7", GaussDBVersion: "8.1", Source: "db://mysql", Target: "db://gauss"},
		ResourcePeaks: ResourcePeaks{MySQLThreadsConnected: 8, MySQLThreadsRunning: 4, GaussDBSessions: 6},
		Workloads:     []WorkloadResult{{Name: "full_then_cdc/steady-1000tps", TargetTPS: 1000, ActualTPS: 900.5, Committed: 108000, Errors: 2, FailureClass: failureCapacity, ErrorSamples: []string{"Error 1213 (40001): Deadlock found"}, LatencyMS: []float64{1, 2, 3}}},
		Verification:  Verification{Match: false, ScopeIsolation: true, Mismatches: 1}, Errors: []string{"raw database error: code=1213"},
		FailedScenario: "full_then_cdc/steady-1000tps", FailureClass: failureCapacity, RecoveryAttempts: 2,
		SkippedLegacy: []string{"full_then_cdc/transaction-boundaries"},
	}
	markdown := renderMarkdownReport(report)
	for _, want := range []string{"CDC 压力测试报告", "测试结果：**失败**", "负载性能基线", "稳态负载（1000 TPS）", "数据库容量边界", "断点与恢复", "恢复请求次数：2", "事务边界", "数据一致：否", "迁移范围隔离正确：是", "2026年07月22日 14:00:00 CST", "Error 1213 (40001): Deadlock found", "raw database error: code=1213"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, markdown)
		}
	}
	for _, unwanted := range []string{"CDC stress report", "Result:", "PASS", "FAIL", "Workload baseline", "Verification", "## Errors", "Match: true", "Match: false"} {
		if strings.Contains(markdown, unwanted) {
			t.Errorf("markdown unexpectedly contains %q:\n%s", unwanted, markdown)
		}
	}
}

func TestChineseStatusText(t *testing.T) {
	if chineseResult(true) != "通过" || chineseResult(false) != "失败" || chineseBool(true) != "是" || chineseBool(false) != "否" {
		t.Fatal("unexpected Chinese status text")
	}
}
