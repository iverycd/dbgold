package cdc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

var ErrDDLPause = errors.New("source DDL requires manual acknowledgement")
var ErrCutoverReady = errors.New("cutover boundary reached")
var ErrBootstrapReview = errors.New("bootstrap exclusions require manual review")
var ddlPrefix = regexp.MustCompile(`(?is)^\s*(CREATE|ALTER|DROP|RENAME|TRUNCATE)\s+`)

type Runner struct {
	cfg   Config
	hooks Hooks
}

func NewRunner(cfg Config, hooks Hooks) *Runner { return &Runner{cfg: cfg, hooks: hooks} }

func (r *Runner) Run(ctx context.Context) error {
	if r.hooks.Status != nil {
		r.hooks.Status("initializing", "initializing", "")
	}
	src, err := OpenSource(r.cfg.SourceDSN)
	if err != nil {
		return err
	}
	defer src.Close()
	tables, err := LoadConfiguredTables(ctx, src, r.cfg)
	if err != nil {
		return err
	}
	byName := tableMap(tables)
	applier, err := newTargetApplier(ctx, r.cfg)
	if err != nil {
		return err
	}
	defer applier.Close()
	record, recordExists, err := applier.LoadBootstrapRecord(ctx)
	if err != nil {
		return err
	}
	if recordExists {
		if record.LocatorStrategyVersion != LocatorStrategyVersion {
			return fmt.Errorf("CDC 定位策略版本不兼容: checkpoint=%d current=%d", record.LocatorStrategyVersion, LocatorStrategyVersion)
		}
		if err = ApplyLocatorStrategies(tables, record.LocatorStrategies); err != nil {
			return err
		}
	} else {
		if r.cfg.LocatorStrategyVersion != LocatorStrategyVersion {
			return fmt.Errorf("任务缺少当前版本的 CDC 定位策略")
		}
		if err = ApplyLocatorStrategies(tables, r.cfg.LocatorStrategies); err != nil {
			return err
		}
		record = BootstrapRecord{State: "completed", Position: r.cfg.Start, EffectiveTables: tableNames(tables), ExcludedTables: r.cfg.ScopeExclusions,
			ManifestHash: r.cfg.ScopeManifestHash, LocatorStrategyVersion: LocatorStrategyVersion, LocatorStrategies: r.cfg.LocatorStrategies}
		if record.ManifestHash == "" {
			record.ManifestHash = HashBootstrapManifest(record)
		}
		if err = applier.SaveBootstrapRecord(ctx, record); err != nil {
			return fmt.Errorf("保存 CDC 定位策略失败: %w", err)
		}
	}
	start := r.cfg.Start
	hasCheckpoint := false
	if cp, ok, e := applier.LoadCheckpoint(ctx); e != nil {
		return e
	} else if ok {
		start = cp
		hasCheckpoint = true
	}
	if start.GTID == "" && start.File == "" {
		return fmt.Errorf("缺少 CDC 起始位点")
	}
	if !hasCheckpoint {
		if err := applier.SaveCheckpoint(ctx, start); err != nil {
			return err
		}
	}

	cfg := replication.BinlogSyncerConfig{ServerID: r.cfg.ServerID, Flavor: "mysql", Host: r.cfg.SourceHost, Port: r.cfg.SourcePort,
		User: r.cfg.SourceUser, Password: r.cfg.SourcePassword, ParseTime: true, HeartbeatPeriod: 10 * time.Second, ReadTimeout: 60 * time.Second,
		MaxReconnectAttempts: 0, Logger: slog.Default()}
	syncer := replication.NewBinlogSyncer(cfg)
	defer syncer.Close()
	var streamer *replication.BinlogStreamer
	if start.GTID != "" {
		set, e := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, start.GTID)
		if e != nil {
			return fmt.Errorf("GTID 格式错误: %w", e)
		}
		streamer, err = syncer.StartSyncGTID(set)
	} else {
		streamer, err = syncer.StartSync(gomysql.Position{Name: start.File, Pos: start.Pos})
	}
	if err != nil {
		return err
	}
	pos := start
	applied := start
	useGTID := start.GTID != ""
	currentGTID := start.GTID
	var changes []Change
	totals := Stats{Position: start}
	head := Position{}
	lastHeadCheck := time.Time{}
	eventCtx := ctx
	gracefulStop := false
	lastStatsPublish := time.Time{}
	lastStatus := ""
	emitStatus := func(status, phase, summary string) {
		if r.hooks.Status != nil && status != lastStatus {
			r.hooks.Status(status, phase, summary)
			lastStatus = status
		}
	}
	emitStats := func(force bool) {
		if r.hooks.Stats != nil && (force || lastStatsPublish.IsZero() || time.Since(lastStatsPublish) >= time.Second) {
			r.hooks.Stats(totals)
			lastStatsPublish = time.Now()
		}
	}
	publish := func(eventTime time.Time, force bool) error {
		if !eventTime.IsZero() {
			totals.LastEventAt = eventTime
		}
		if force || time.Since(lastHeadCheck) >= time.Second {
			latest, e := CurrentPosition(eventCtx, src)
			if e != nil {
				return e
			}
			head, lastHeadCheck = latest, time.Now()
		}
		totals.Position, totals.SourceHead = applied, head
		totals.CaughtUp = PositionReached(applied, head)
		if totals.CaughtUp {
			totals.LagSeconds = 0
		} else if !totals.LastEventAt.IsZero() {
			totals.LagSeconds = max(0, int64(time.Since(totals.LastEventAt).Seconds()))
		}
		if boundary, ok := Registry.CutoverBoundary(r.cfg.JobID); ok {
			reached := PositionReached(applied, boundary)
			emitStats(force || gracefulStop || reached)
			emitStatus("cutting_over", "cutting_over", "正在追赶切换边界")
			if reached {
				return ErrCutoverReady
			}
		} else {
			emitStats(force || gracefulStop)
			if totals.CaughtUp {
				emitStatus("running", "running", "已追平，持续同步中")
			} else {
				emitStatus("catching_up", "catching_up", "正在追赶源库增量")
			}
		}
		return nil
	}
	var graceCancel context.CancelFunc
	// Publish a real source watermark before waiting for the first event. This
	// also marks an idle source as caught up instead of waiting indefinitely.
	if latest, e := CurrentPosition(ctx, src); e != nil {
		return e
	} else {
		head = latest
		lastHeadCheck = time.Now()
	}
	if err := publish(time.Time{}, false); err != nil {
		return err
	}
	commit := func(eventTime time.Time) error {
		if PositionReached(applied, pos) {
			// go-mysql may replay the most recently committed GTID after an
			// internal reconnect. The target checkpoint is authoritative, so a
			// replayed transaction (especially an INSERT into a no-PK table) must
			// not be applied twice.
			changes = nil
		} else {
			stats, e := applier.Apply(eventCtx, changes, pos)
			if e != nil {
				var conflict *RowConflictError
				if errors.As(e, &conflict) && r.hooks.RowConflict != nil {
					r.hooks.RowConflict(RowConflict{Table: conflict.Table, Action: conflict.Action, Position: pos, Error: conflict.Error(), BeforeHash: conflict.BeforeHash})
				}
				totals.Position, totals.SourceHead = applied, head
				emitStats(true)
				return e
			}
			changes = nil
			totals = addStats(totals, stats)
			applied = pos
		}
		if err := publish(eventTime, false); err != nil {
			totals.Position, totals.SourceHead = applied, head
			emitStats(true)
			return err
		}
		if gracefulStop {
			return ctx.Err()
		}
		return nil
	}
	for {
		ev, err := streamer.GetEvent(eventCtx)
		if err != nil {
			if ctx.Err() != nil && !gracefulStop && len(changes) > 0 {
				// Pause/stop only after the source transaction currently buffered is
				// atomically applied. A timeout still leaves the previous checkpoint
				// valid, so a later resume safely replays the transaction.
				eventCtx, graceCancel = context.WithTimeout(context.Background(), 5*time.Minute)
				defer graceCancel()
				gracefulStop = true
				continue
			}
			if ctx.Err() != nil {
				totals.Position, totals.SourceHead = applied, head
				emitStats(true)
				return ctx.Err()
			}
			totals.Position, totals.SourceHead = applied, head
			emitStats(true)
			return err
		}
		if ev.Header.LogPos > 0 {
			pos.Pos = ev.Header.LogPos
		}
		switch e := ev.Event.(type) {
		case *replication.RotateEvent:
			pos.File = string(e.NextLogName)
			pos.Pos = uint32(e.Position)
		case *replication.GTIDEvent:
			nextGTID := fmt.Sprintf("%s:%d", formatSID(e.SID), e.GNO)
			if nextGTID == currentGTID && len(changes) > 0 {
				// Reconnect during a GTID transaction restarts that transaction
				// from its GTID event; discard the partial pre-disconnect buffer.
				changes = nil
			}
			currentGTID = nextGTID
		case *replication.RowsEvent:
			if string(e.Table.Schema) != r.cfg.SourceDatabase {
				continue
			}
			t := byName[string(e.Table.Table)]
			if t == nil {
				continue
			}
			switch e.Type() {
			case replication.EnumRowsEventTypeInsert:
				for _, row := range e.Rows {
					changes = append(changes, Change{Action: "insert", Table: t, After: row})
				}
			case replication.EnumRowsEventTypeDelete:
				for _, row := range e.Rows {
					changes = append(changes, Change{Action: "delete", Table: t, Before: row})
				}
			case replication.EnumRowsEventTypeUpdate:
				for i := 0; i+1 < len(e.Rows); i += 2 {
					changes = append(changes, Change{Action: "update", Table: t, Before: e.Rows[i], After: e.Rows[i+1]})
				}
			}
		case *replication.XIDEvent:
			if useGTID && e.GSet != nil {
				pos.GTID = e.GSet.String()
			} else if useGTID && currentGTID != "" {
				pos.GTID = mergeGTID(pos.GTID, currentGTID)
			}
			eventTime := time.Time{}
			if ev.Header.Timestamp > 0 {
				eventTime = time.Unix(int64(ev.Header.Timestamp), 0)
			}
			if err := commit(eventTime); err != nil {
				return err
			}
		case *replication.QueryEvent:
			query := strings.TrimSpace(string(e.Query))
			upper := strings.ToUpper(query)
			if ddlPrefix.MatchString(query) && (string(e.Schema) == r.cfg.SourceDatabase || strings.Contains(query, r.cfg.SourceDatabase)) {
				if useGTID {
					pos.GTID = mergeGTID(pos.GTID, currentGTID)
				}
				if r.hooks.DDL != nil {
					totals.Position, totals.SourceHead = applied, head
					emitStats(true)
					r.hooks.DDL(query, pos)
				}
				return ErrDDLPause
			}
			if upper == "BEGIN" || strings.HasPrefix(upper, "SAVEPOINT") || strings.HasPrefix(upper, "XA ") {
				break
			}
			if upper == "ROLLBACK" {
				changes = nil
			}
			if upper == "COMMIT" || upper == "ROLLBACK" || len(changes) == 0 {
				if useGTID {
					pos.GTID = mergeGTID(pos.GTID, currentGTID)
				}
				eventTime := time.Time{}
				if ev.Header.Timestamp > 0 {
					eventTime = time.Unix(int64(ev.Header.Timestamp), 0)
				}
				if err := commit(eventTime); err != nil {
					return err
				}
			}
		}
		if time.Since(lastHeadCheck) >= time.Second {
			if err := publish(time.Time{}, true); err != nil {
				totals.Position, totals.SourceHead = applied, head
				emitStats(true)
				return err
			}
		}
	}
}

func tableNames(tables []TableInfo) []string {
	result := make([]string, len(tables))
	for i := range tables {
		result[i] = tables[i].Name
	}
	return result
}

// PositionReached reports whether applied includes the requested GTID set or
// has reached/passed the requested file position.
func PositionReached(applied, requested Position) bool {
	if requested.GTID != "" && applied.GTID != "" {
		a, e1 := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, applied.GTID)
		b, e2 := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, requested.GTID)
		return e1 == nil && e2 == nil && a.Contain(b)
	}
	if requested.File == "" {
		return true
	}
	if applied.File == requested.File {
		return applied.Pos >= requested.Pos
	}
	ai, aok := binlogIndex(applied.File)
	bi, bok := binlogIndex(requested.File)
	if aok && bok {
		return ai > bi
	}
	return applied.File > requested.File
}

func PositionEquivalent(a, b Position) bool {
	if a.File != "" && b.File != "" && (a.File != b.File || a.Pos != b.Pos) {
		return false
	}
	if a.GTID != "" && b.GTID != "" {
		as, e1 := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, a.GTID)
		bs, e2 := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, b.GTID)
		return e1 == nil && e2 == nil && as.Equal(bs)
	}
	if a.File == "" || b.File == "" {
		return false
	}
	return a.File == b.File && a.Pos == b.Pos
}

func binlogIndex(file string) (uint64, bool) {
	i := strings.LastIndex(file, ".")
	if i < 0 {
		return 0, false
	}
	n, e := strconv.ParseUint(file[i+1:], 10, 64)
	return n, e == nil
}

func formatSID(sid []byte) string {
	h := hex.EncodeToString(sid)
	if len(h) != 32 {
		return h
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func mergeGTID(set, one string) string {
	if one == "" {
		return set
	}
	if set == "" {
		return one
	}
	g, e := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, set)
	if e != nil {
		return set
	}
	if e = g.Update(one); e != nil {
		return set
	}
	return g.String()
}
func addStats(a, b Stats) Stats {
	a.Inserts += b.Inserts
	a.Updates += b.Updates
	a.Deletes += b.Deletes
	a.Skipped += b.Skipped
	a.Warnings += b.Warnings
	a.Position = b.Position
	a.LastEventAt = b.LastEventAt
	return a
}

func AcknowledgeDDL(ctx context.Context, cfg Config, p Position) error {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SaveCheckpoint(ctx, p)
}

func SaveTargetCheckpoint(ctx context.Context, cfg Config, p Position) error {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SaveCheckpoint(ctx, p)
}

func LoadTargetCheckpoint(ctx context.Context, cfg Config) (Position, bool, error) {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return Position{}, false, e
	}
	defer a.Close()
	return a.LoadCheckpoint(ctx)
}

func SaveTargetBootstrapRecord(ctx context.Context, cfg Config, record BootstrapRecord) error {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SaveBootstrapRecord(ctx, record)
}

func LoadTargetBootstrapRecord(ctx context.Context, cfg Config) (BootstrapRecord, bool, error) {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return BootstrapRecord{}, false, e
	}
	defer a.Close()
	return a.LoadBootstrapRecord(ctx)
}

func FinalizeTargetBootstrap(ctx context.Context, cfg Config, record BootstrapRecord) error {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.FinalizeBootstrap(ctx, record)
}

func AbortTargetBootstrap(ctx context.Context, cfg Config) error {
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.MarkBootstrapAborted(ctx)
}

func SyncSequences(ctx context.Context, cfg Config) error {
	src, e := OpenSource(cfg.SourceDSN)
	if e != nil {
		return e
	}
	defer src.Close()
	tables, e := LoadConfiguredTables(ctx, src, cfg)
	if e != nil {
		return e
	}
	a, e := newTargetApplier(ctx, cfg)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SyncSequences(ctx, tables)
}
