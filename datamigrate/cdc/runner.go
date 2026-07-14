package cdc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

var ErrDDLPause = errors.New("source DDL requires manual acknowledgement")
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
	tables, err := LoadTables(ctx, src, r.cfg.SourceDatabase, r.cfg.Mode, r.cfg.Filter)
	if err != nil {
		return err
	}
	byName := tableMap(tables)
	applier, err := NewPostgresApplier(ctx, r.cfg.TargetDSN, r.cfg.TargetSchema, r.cfg.JobID, r.cfg.LowerCaseNames)
	if err != nil {
		return err
	}
	defer applier.Close()
	start := r.cfg.Start
	if cp, ok, e := applier.LoadCheckpoint(ctx); e != nil {
		return e
	} else if ok {
		start = cp
	}
	if start.GTID == "" && start.File == "" {
		return fmt.Errorf("缺少 CDC 起始位点")
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
	if r.hooks.Status != nil {
		r.hooks.Status("running", "running", "")
	}
	pos := start
	currentGTID := start.GTID
	var changes []Change
	var totals Stats
	eventCtx := ctx
	gracefulStop := false
	var graceCancel context.CancelFunc
	defer func() {
		if graceCancel != nil {
			graceCancel()
		}
	}()
	for {
		ev, err := streamer.GetEvent(eventCtx)
		if err != nil {
			if ctx.Err() != nil && !gracefulStop && len(changes) > 0 {
				// Pause/stop only after the source transaction currently buffered is
				// atomically applied. A timeout still leaves the previous checkpoint
				// valid, so a later resume safely replays the transaction.
				eventCtx, graceCancel = context.WithTimeout(context.Background(), 5*time.Minute)
				gracefulStop = true
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
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
			currentGTID = fmt.Sprintf("%s:%d", formatSID(e.SID), e.GNO)
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
			if e.GSet != nil {
				pos.GTID = e.GSet.String()
			} else if currentGTID != "" {
				pos.GTID = mergeGTID(pos.GTID, currentGTID)
			}
			stats, e2 := applier.Apply(eventCtx, changes, pos)
			if e2 != nil {
				return e2
			}
			changes = nil
			totals = addStats(totals, stats)
			if ev.Header.Timestamp > 0 {
				totals.LastEventAt = time.Unix(int64(ev.Header.Timestamp), 0)
			}
			if r.hooks.Stats != nil {
				r.hooks.Stats(totals)
			}
			if gracefulStop {
				return ctx.Err()
			}
		case *replication.QueryEvent:
			query := strings.TrimSpace(string(e.Query))
			upper := strings.ToUpper(query)
			if ddlPrefix.MatchString(query) && (string(e.Schema) == r.cfg.SourceDatabase || strings.Contains(query, r.cfg.SourceDatabase)) {
				pos.GTID = mergeGTID(pos.GTID, currentGTID)
				if r.hooks.DDL != nil {
					r.hooks.DDL(query, pos)
				}
				return ErrDDLPause
			}
			if upper == "COMMIT" && len(changes) > 0 {
				pos.GTID = mergeGTID(pos.GTID, currentGTID)
				stats, e2 := applier.Apply(eventCtx, changes, pos)
				if e2 != nil {
					return e2
				}
				changes = nil
				totals = addStats(totals, stats)
				if r.hooks.Stats != nil {
					r.hooks.Stats(totals)
				}
				if gracefulStop {
					return ctx.Err()
				}
			}
		}
	}
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
	a, e := NewPostgresApplier(ctx, cfg.TargetDSN, cfg.TargetSchema, cfg.JobID, cfg.LowerCaseNames)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SaveCheckpoint(ctx, p)
}
func SyncSequences(ctx context.Context, cfg Config) error {
	src, e := OpenSource(cfg.SourceDSN)
	if e != nil {
		return e
	}
	defer src.Close()
	tables, e := LoadTables(ctx, src, cfg.SourceDatabase, cfg.Mode, cfg.Filter)
	if e != nil {
		return e
	}
	a, e := NewPostgresApplier(ctx, cfg.TargetDSN, cfg.TargetSchema, cfg.JobID, cfg.LowerCaseNames)
	if e != nil {
		return e
	}
	defer a.Close()
	return a.SyncSequences(ctx, tables)
}
