package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

const (
	incrementalLogBatchSize     = 100
	incrementalLogFlushInterval = 500 * time.Millisecond
	incrementalLogDefaultLimit  = 500
	incrementalLogMaximumLimit  = 1000
)

var (
	appendIncrementalLogRows = store.AppendIncrementalMigrationLogs
	incrementalLogLevels     = map[string]string{
		"[INFO]": "info", "[DDL]": "ddl", "[DATA]": "data", "[INDEX]": "index",
		"[WARN]": "warn", "[ERROR]": "error", "[DONE]": "done",
	}
	sensitiveLogPatterns = []struct {
		re          *regexp.Regexp
		replacement string
	}{
		{regexp.MustCompile(`(?i)(invalid (?:input syntax|input value)[^:\n]*:\s*)(?:"[^"\n]*"|'[^'\n]*')`), `${1}"<redacted>"`},
		{regexp.MustCompile(`(?i)(date/time field value out of range:\s*)(?:"[^"\n]*"|'[^'\n]*')`), `${1}"<redacted>"`},
		{regexp.MustCompile(`(?i)(incorrect [^:\n]+ value:\s*)(?:"[^"\n]*"|'[^'\n]*')`), `${1}"<redacted>"`},
		{regexp.MustCompile(`(?i)(COPY [^,\n]+,\s*line \d+,\s*column [^:\n]+:\s*)(?:"[^"\n]*"|'[^'\n]*')`), `${1}"<redacted>"`},
		{regexp.MustCompile(`(?i)(Key \([^\n)]*\)=\()[^\n)]*(\))`), `${1}<redacted>${2}`},
	}
)

// startIncrementalLogJournal drains Migrator's non-blocking log channel,
// preserving ordering while batching SQLite writes. The returned channel is
// resolved only after the final batch has been flushed.
func startIncrementalLogJournal(jobID string, job *datamigrate.Job) <-chan int64 {
	done := make(chan int64, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(incrementalLogFlushInterval)
		defer ticker.Stop()

		phase := "snapshot_init"
		batch := make([]store.IncrementalMigrationLog, 0, incrementalLogBatchSize)
		var persistDropped int64
		var latestLine string
		var persistedSummaryLine string
		lastSummary := time.Time{}
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := appendIncrementalLogBatchWithRetry(batch); err != nil {
				persistDropped += int64(len(batch))
			}
			batch = batch[:0]
		}
		persistSummary := func(force bool) {
			if latestLine == "" || latestLine == persistedSummaryLine || (!force && !lastSummary.IsZero() && time.Since(lastSummary) < 2*time.Second) {
				return
			}
			if err := store.UpdateIncrementalJob(jobID, map[string]any{"summary": "全量快照：" + latestLine}); err == nil {
				persistedSummaryLine = latestLine
			}
			lastSummary = time.Now()
		}

		for {
			select {
			case line, ok := <-job.LogCh:
				if !ok {
					flush()
					persistSummary(true)
					totalDropped := persistDropped + int64(job.DroppedLogCount())
					if totalDropped > 0 {
						_ = addIncrementalLogDroppedCountWithRetry(jobID, totalDropped)
						warning := newIncrementalLog(jobID, phase, "warn", fmt.Sprintf("日志通道或持久化拥塞，%d 条全量日志未能保存", totalDropped))
						if err := appendIncrementalLogBatchWithRetry([]store.IncrementalMigrationLog{warning}); err != nil {
							_ = addIncrementalLogDroppedCountWithRetry(jobID, 1)
							totalDropped++
						}
					}
					done <- totalDropped
					return
				}
				line = sanitizeIncrementalLogLine(line)
				phase = incrementalLogPhase(line, phase)
				latestLine = line
				batch = append(batch, store.IncrementalMigrationLog{
					JobID: jobID, Phase: phase, Level: incrementalLogLevel(line),
					Line: line, CreatedAt: time.Now(),
				})
				if len(batch) >= incrementalLogBatchSize {
					flush()
				}
				persistSummary(false)
			case <-ticker.C:
				flush()
				persistSummary(false)
			}
		}
	}()
	return done
}

func appendIncrementalLifecycleLog(jobID, phase, level, message string) {
	entry := newIncrementalLog(jobID, phase, level, message)
	if err := appendIncrementalLogBatchWithRetry([]store.IncrementalMigrationLog{entry}); err != nil {
		_ = addIncrementalLogDroppedCountWithRetry(jobID, 1)
	}
}

func newIncrementalLog(jobID, phase, level, message string) store.IncrementalMigrationLog {
	level = strings.ToLower(level)
	now := time.Now()
	return store.IncrementalMigrationLog{
		JobID: jobID, Phase: phase, Level: level,
		Line:      sanitizeIncrementalLogLine(now.Format("15:04:05.000") + " [" + strings.ToUpper(level) + "] " + message),
		CreatedAt: now,
	}
}

func appendIncrementalLogBatchWithRetry(batch []store.IncrementalMigrationLog) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = appendIncrementalLogRows(batch); err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(50*(1<<attempt)) * time.Millisecond)
		}
	}
	return err
}

func addIncrementalLogDroppedCountWithRetry(jobID string, count int64) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = store.AddIncrementalLogDroppedCount(jobID, count); err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(50*(1<<attempt)) * time.Millisecond)
		}
	}
	return err
}

func incrementalLogPhase(line, current string) string {
	level, message := incrementalLogParts(line)
	if level != "info" {
		return current
	}
	switch {
	case strings.HasPrefix(message, "=== Phase 1:"):
		return "snapshot_schema"
	case strings.HasPrefix(message, "=== Phase 2:"):
		return "snapshot_data"
	case strings.HasPrefix(message, "=== Phase 3:"):
		return "snapshot_objects"
	case strings.HasPrefix(message, "=== Phase 4:"):
		return "snapshot_validation"
	default:
		return current
	}
}

func incrementalLogLevel(line string) string {
	level, _ := incrementalLogParts(line)
	if level != "" {
		return level
	}
	return "info"
}

func incrementalLogParts(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", ""
	}
	markerIndex := 0
	if _, err := time.Parse("15:04:05.000", fields[0]); err == nil {
		markerIndex = 1
	}
	if markerIndex >= len(fields) {
		return "", trimmed
	}
	marker := fields[markerIndex]
	level, ok := incrementalLogLevels[marker]
	if !ok {
		return "", trimmed
	}
	position := strings.Index(trimmed, marker)
	if position < 0 {
		return level, ""
	}
	return level, strings.TrimSpace(trimmed[position+len(marker):])
}

func sanitizeIncrementalLogLine(line string) string {
	for _, pattern := range sensitiveLogPatterns {
		line = pattern.re.ReplaceAllString(line, pattern.replacement)
	}
	return line
}

func GetIncrementalLogs(c *gin.Context) {
	job, ok := ownedIncremental(c)
	if !ok {
		return
	}
	afterRaw, hasAfter := c.GetQuery("after_id")
	beforeRaw, hasBefore := c.GetQuery("before_id")
	if hasAfter && hasBefore {
		c.JSON(http.StatusBadRequest, gin.H{"error": "after_id 与 before_id 不能同时使用"})
		return
	}
	afterID, err := parsePositiveLogCursor(afterRaw, hasAfter)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "after_id 必须是正整数"})
		return
	}
	beforeID, err := parsePositiveLogCursor(beforeRaw, hasBefore)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "before_id 必须是正整数"})
		return
	}
	limit := incrementalLogDefaultLimit
	if raw, exists := c.GetQuery("limit"); exists {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 1 || value > incrementalLogMaximumLimit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit 必须在 1 到 1000 之间"})
			return
		}
		limit = value
	}
	page, err := store.ListIncrementalMigrationLogs(job.JobID, afterID, beforeID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取增量任务日志失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items": page.Items, "oldest_id": page.OldestID, "newest_id": page.NewestID,
		"has_older": page.HasOlder, "has_newer": page.HasNewer,
		"log_dropped_count": job.LogDroppedCount,
	})
}

func parsePositiveLogCursor(raw string, present bool) (uint64, error) {
	if !present {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return value, nil
}
