package cdc

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
)

// queryBody strips leading comments, preserving executable MySQL comments.
func queryBody(query string) string {
	for {
		query = strings.TrimSpace(query)
		if strings.HasPrefix(query, "/*") {
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return query
			}
			end += 2
			if strings.HasPrefix(query, "/*!") {
				body := strings.TrimLeft(query[3:end], "0123456789")
				query = body + " " + query[end+2:]
			} else {
				query = query[end+2:]
			}
			continue
		}
		if strings.HasPrefix(query, "#") || (strings.HasPrefix(query, "--") && (len(query) == 2 || query[2] <= ' ')) {
			end := strings.IndexByte(query, '\n')
			if end < 0 {
				return ""
			}
			query = query[end+1:]
			continue
		}
		return query
	}
}
func sourceDDL(query, schema, database string) bool {
	q := queryBody(query)
	// Unknown/default-less DDL is treated conservatively. Never claim a schema
	// change is unrelated merely because it used qualified identifiers.
	switch queryKeyword(q) {
	case "CREATE", "ALTER", "DROP", "RENAME", "TRUNCATE":
		return schema == "" || strings.EqualFold(schema, database) || strings.Contains(strings.ToLower(strings.ReplaceAll(q, "``", "`")), strings.ToLower(database))
	default:
		return false
	}
}

// A comment can delimit a keyword without any whitespace (XA/*...*/START).
func queryKeyword(query string) string {
	q := queryBody(query)
	end := 0
	for end < len(q) && ((q[end] >= 'a' && q[end] <= 'z') || (q[end] >= 'A' && q[end] <= 'Z') || q[end] == '_') {
		end++
	}
	return strings.ToUpper(q[:end])
}
func checkUnsupportedBinlogEvent(ev *replication.BinlogEvent, p Position, selectedRows bool) error {
	kind := ""
	switch ev.Header.EventType {
	case replication.TRANSACTION_PAYLOAD_EVENT:
		kind = "TRANSACTION_PAYLOAD_EVENT（压缩事务）"
	case replication.XA_PREPARE_LOG_EVENT:
		kind = "XA_PREPARE_LOG_EVENT（显式 XA 事务）"
	case replication.PARTIAL_UPDATE_ROWS_EVENT:
		if selectedRows {
			kind = "PARTIAL_UPDATE_ROWS_EVENT（PARTIAL_JSON）"
		}
	}
	switch event := ev.Event.(type) {
	case *replication.QueryEvent:
		if queryKeyword(string(event.Query)) == "XA" {
			kind = "XA 查询事件"
		}
	case *replication.RowsEvent:
		if selectedRows && kind == "" && hasPartialRowImage(event) {
			kind = "部分行镜像（要求 binlog_row_image=FULL）"
		}
	}
	if kind != "" {
		return fmt.Errorf("CDC 不支持 %s，源位点 %s:%d（GTID=%s）；原检查点保留。关闭配置或修改当前数据不会改变历史 binlog，请重新建立全量或经过核验的增量起点", kind, p.File, ev.Header.LogPos, p.GTID)
	}
	return nil
}

func hasPartialRowImage(event *replication.RowsEvent) bool {
	for _, bitmap := range [][]byte{event.ColumnBitmap1, event.ColumnBitmap2} {
		if len(bitmap) == 0 {
			continue
		}
		for i := uint64(0); i < event.ColumnCount; i++ {
			if i/8 >= uint64(len(bitmap)) || bitmap[i/8]&(1<<(i%8)) == 0 {
				return true
			}
		}
	}
	for _, skipped := range event.SkippedColumns {
		if len(skipped) > 0 {
			return true
		}
	}
	return false
}

type transactionEvent struct {
	Commit           bool
	MetadataProgress bool
	DDL              string
	Time             time.Time
}

// transactionState contains only binlog state, with no I/O. A caller may only
// write Changes and Position on Commit, or Position alone on MetadataProgress.
type transactionState struct {
	Position        Position
	Changes         []Change
	currentGTID     string
	useGTID         bool
	database        string
	tables          map[string]*TableInfo
	transactionOpen bool
	rotationPending bool
}

func newTransactionState(cfg Config, tables []TableInfo, start Position) *transactionState {
	return &transactionState{Position: start, useGTID: start.GTID != "", database: cfg.SourceDatabase, tables: tableMap(tables)}
}
func (s *transactionState) Consume(ev *replication.BinlogEvent) (transactionEvent, error) {
	out := transactionEvent{}
	at := s.Position
	if s.currentGTID != "" {
		at.GTID = s.currentGTID
	}
	if s.useGTID && ev.Header.EventType == replication.GTID_TAGGED_LOG_EVENT {
		s.Changes = nil
		return out, unsupportedTaggedGTID(at, ev.Header.LogPos)
	}
	var table *TableInfo
	if rows, ok := ev.Event.(*replication.RowsEvent); ok && rows.Table != nil && string(rows.Table.Schema) == s.database {
		table = s.tables[string(rows.Table.Table)]
	}
	if err := checkUnsupportedBinlogEvent(ev, at, table != nil); err != nil {
		s.Changes = nil
		return out, err
	}
	if ev.Header.Timestamp > 0 {
		out.Time = time.Unix(int64(ev.Header.Timestamp), 0)
	}
	previous := s.Position
	if s.rotationPending {
		switch ev.Header.EventType {
		case replication.ROTATE_EVENT, replication.FORMAT_DESCRIPTION_EVENT, replication.PREVIOUS_GTIDS_EVENT, replication.HEARTBEAT_EVENT, replication.HEARTBEAT_LOG_EVENT_V2:
		default:
			s.rotationPending = false
		}
	}
	if ev.Header.LogPos > 0 {
		s.Position.Pos = ev.Header.LogPos
	}
	switch e := ev.Event.(type) {
	case *replication.RotateEvent:
		nextFile := string(e.NextLogName)
		if s.transactionOpen && previous.File != "" && previous.File != nextFile {
			s.Position = previous
			return out, fmt.Errorf("源事务尚未结束时 binlog 从 %s 轮转到 %s；原检查点保留", previous.File, nextFile)
		}
		s.Position.File = nextFile
		s.Position.Pos = uint32(e.Position)
		// A synthetic rotation announces a file after connecting or reconnecting.
		// It does not prove that any bytes in a new file were applied.
		// MySQL may repeat the same rotation synthetically before streaming the
		// next file's header. Retain only the candidate proven by a real event.
		repeated := s.rotationPending && previous == s.Position
		rotated := previous.File != nextFile && !PositionReached(previous, s.Position) && realBinlogMetadata(ev.Header)
		s.rotationPending = !s.useGTID && !s.transactionOpen && e.Position == 4 && previous.File != "" && (repeated || rotated)
	case *replication.FormatDescriptionEvent, *replication.PreviousGTIDsEvent:
		out.MetadataProgress = s.rotationPending && !s.transactionOpen && len(s.Changes) == 0 &&
			realBinlogMetadata(ev.Header) && ev.Header.EventSize != 0 &&
			uint64(previous.Pos)+uint64(ev.Header.EventSize) == uint64(ev.Header.LogPos)
		// A non-contiguous or synthetic position cannot seed another update.
		if !out.MetadataProgress {
			s.Position = previous
		}
	case *replication.GTIDEvent:
		next := fmt.Sprintf("%s:%d", formatSID(e.SID), e.GNO)
		if next == s.currentGTID {
			s.Changes = nil
		}
		s.currentGTID = next
		s.transactionOpen = true
	case *replication.GtidTaggedLogEvent:
		s.transactionOpen = true
	case *replication.TableMapEvent:
		s.transactionOpen = true
	case *replication.RowsEvent:
		s.transactionOpen = true
		if table == nil {
			break
		}
		switch e.Type() {
		case replication.EnumRowsEventTypeInsert:
			for _, row := range e.Rows {
				s.Changes = append(s.Changes, Change{Action: "insert", Table: table, After: row})
			}
		case replication.EnumRowsEventTypeDelete:
			for _, row := range e.Rows {
				s.Changes = append(s.Changes, Change{Action: "delete", Table: table, Before: row})
			}
		case replication.EnumRowsEventTypeUpdate:
			if len(e.Rows)%2 != 0 {
				return out, fmt.Errorf("表 %s 的 UPDATE 行镜像不完整", table.Name)
			}
			for i := 0; i < len(e.Rows); i += 2 {
				s.Changes = append(s.Changes, Change{Action: "update", Table: table, Before: e.Rows[i], After: e.Rows[i+1]})
			}
		default:
			return out, fmt.Errorf("表 %s 的 binlog 行事件类型 %s 不支持，原检查点保留", table.Name, ev.Header.EventType)
		}
	case *replication.XIDEvent:
		if s.useGTID {
			if e.GSet != nil {
				s.Position.GTID = e.GSet.String()
			} else {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
		}
		s.transactionOpen = false
		out.Commit = true
	case *replication.QueryEvent:
		query := queryBody(string(e.Query))
		upper := strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(query, ";")))
		if sourceDDL(query, string(e.Schema), s.database) {
			if s.useGTID {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
			out.DDL = string(e.Query)
			return out, nil
		}
		if upper == "BEGIN" {
			s.transactionOpen = true
			break
		}
		if strings.HasPrefix(upper, "SAVEPOINT") || strings.HasPrefix(upper, "RELEASE ") || strings.HasPrefix(upper, "ROLLBACK TO ") {
			break
		}
		if upper == "ROLLBACK" {
			s.Changes = nil
		}
		if upper == "COMMIT" || upper == "ROLLBACK" || len(s.Changes) == 0 {
			if s.useGTID {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
			s.transactionOpen = false
			out.Commit = true
		}
	case *replication.HeartbeatEvent:
		// Heartbeats report the source's view, not a consumed physical position.
		s.Position = previous
	}
	return out, nil
}

func realBinlogMetadata(header *replication.EventHeader) bool {
	return header.Timestamp != 0 && header.Flags&replication.LOG_EVENT_ARTIFICIAL_F == 0
}

func unsupportedTaggedGTID(p Position, end uint32) error {
	return fmt.Errorf("GTID 模式暂不支持 GTID_TAGGED_LOG_EVENT，源位点 %s:%d；原检查点保留", p.File, end)
}
