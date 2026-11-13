package cdc

import (
	"context"
	"fmt"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

// Scan raw events: validating a schema must not decode historical rows with
// current metadata, nor decompress a potentially enormous unsupported payload.
func validateSchemaInterval(ctx context.Context, cfg Config, start, head Position) error {
	if PositionReached(start, head) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	syncer := replication.NewBinlogSyncer(replication.BinlogSyncerConfig{
		ServerID: cfg.ServerID, Flavor: "mysql", Host: cfg.SourceHost, Port: cfg.SourcePort, User: cfg.SourceUser, Password: cfg.SourcePassword,
		RawModeEnabled: true, VerifyChecksum: true, DisableRetrySync: true, ReadTimeout: 30 * time.Second,
	})
	defer syncer.Close()
	var stream *replication.BinlogStreamer
	var err error
	if start.GTID != "" {
		set, e := gomysql.ParseGTIDSet(gomysql.MySQLFlavor, start.GTID)
		if e != nil {
			return e
		}
		stream, err = syncer.StartSyncGTID(set)
	} else {
		stream, err = syncer.StartSync(gomysql.Position{Name: start.File, Pos: start.Pos})
	}
	if err != nil {
		return fmt.Errorf("核验源结构历史区间失败: %w", err)
	}
	p := start
	boundary := intervalTransactionState{}
	for {
		ev, e := stream.GetEvent(ctx)
		if e != nil {
			return fmt.Errorf("无法确认历史位点对应的源结构，禁止开始写入: %w", e)
		}
		if rotate, ok := ev.Event.(*replication.RotateEvent); ok {
			p.File = string(rotate.NextLogName)
			p.Pos = uint32(rotate.Position)
		}
		if ev.Header.LogPos > 0 {
			p.Pos = ev.Header.LogPos
		}
		if start.GTID != "" {
			// GTID sets can have holes. The GTID stream skips transactions the
			// caller claims to have applied, potentially hiding a later DDL while
			// replaying an earlier row. Locate the first missing transaction, then
			// inspect EVERY physical event from there to the captured head.
			if ev.Header.EventType != replication.GTID_EVENT && ev.Header.EventType != replication.ANONYMOUS_GTID_EVENT {
				continue
			}
			if p.File == "" || ev.Header.LogPos < ev.Header.EventSize+4 {
				return fmt.Errorf("无法确定 GTID 历史起点的物理事务边界，请重新建立起点")
			}
			physical := Position{File: p.File, Pos: ev.Header.LogPos - ev.Header.EventSize}
			syncer.Close()
			return validateSchemaInterval(ctx, cfg, physical, head)
		}
		if ev.Header.EventType == replication.QUERY_EVENT {
			raw, ok := ev.Event.(*replication.GenericEvent)
			if !ok {
				return fmt.Errorf("历史 DDL 事件格式异常")
			}
			query := &replication.QueryEvent{}
			if err = decodeIntervalQuery(query, raw.Data); err != nil {
				return err
			}
			ev.Event = query
			if sourceDDL(string(query.Query), string(query.Schema), cfg.SourceDatabase) {
				return fmt.Errorf("起点之后存在源 DDL，无法用当前结构解释历史 binlog（%s:%d）；请重新建立全量或增量起点", p.File, p.Pos)
			}
		}
		if err = checkDamengEvent(ev, p); err != nil {
			return err
		}
		if err = boundary.consume(ev); err != nil {
			return fmt.Errorf("无法确认历史起点的完整事务边界（%s:%d）: %w", p.File, p.Pos, err)
		}
		// CurrentPosition always supplies physical coordinates, even for GTID
		// starts. Do not use a speculative GTID set to terminate a partial txn.
		if p.File != "" && PositionReached(Position{File: p.File, Pos: p.Pos}, Position{File: head.File, Pos: head.Pos}) {
			if boundary.open {
				return fmt.Errorf("历史区间终点不是完整事务边界，需重新建立起点")
			}
			return nil
		}
	}
}

// A file position may point into the middle of a transaction. Never accept a
// suffix of row events as a new starting point, even when no DDL was found.
type intervalTransactionState struct{ open bool }

func (s *intervalTransactionState) consume(ev *replication.BinlogEvent) error {
	switch ev.Header.EventType {
	case replication.GTID_EVENT, replication.ANONYMOUS_GTID_EVENT:
		if s.open {
			return fmt.Errorf("前一事务缺少提交事件")
		}
		s.open = true
	case replication.QUERY_EVENT:
		q := ev.Event.(*replication.QueryEvent)
		query := strings.TrimSuffix(strings.TrimSpace(queryBody(string(q.Query))), ";")
		switch queryKeyword(query) {
		case "BEGIN":
			s.open = true
		case "SAVEPOINT", "RELEASE":
		case "COMMIT", "ROLLBACK":
			if !s.open {
				return fmt.Errorf("起点位于事务内部，请使用提交后的位点")
			}
			s.open = false
		default:
			s.open = false // stand-alone statement/DDL, including other schemas
		}
	case replication.TABLE_MAP_EVENT, replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2,
		replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2,
		replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2, replication.PARTIAL_UPDATE_ROWS_EVENT:
		if !s.open {
			return fmt.Errorf("起点位于事务内部，请使用提交后的位点")
		}
	case replication.XID_EVENT:
		if !s.open {
			return fmt.Errorf("起点位于事务内部，请使用提交后的位点")
		}
		s.open = false
	}
	return nil
}
func decodeIntervalQuery(q *replication.QueryEvent, data []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("历史 QueryEvent 数据不完整")
		}
	}()
	return q.Decode(data)
}
