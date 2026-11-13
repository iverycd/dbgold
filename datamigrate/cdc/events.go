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
func checkDamengEvent(ev *replication.BinlogEvent, p Position) error {
	kind := ""
	switch ev.Header.EventType {
	case replication.TRANSACTION_PAYLOAD_EVENT:
		kind = "TRANSACTION_PAYLOAD_EVENT（压缩事务）"
	case replication.XA_PREPARE_LOG_EVENT:
		kind = "XA_PREPARE_LOG_EVENT（显式 XA 事务）"
	}
	if q, ok := ev.Event.(*replication.QueryEvent); ok {
		if queryKeyword(string(q.Query)) == "XA" {
			kind = "XA 查询事件"
		}
	}
	if kind != "" {
		return fmt.Errorf("达梦 CDC 不支持 %s，源位点 %s:%d（GTID=%s）；原检查点保留。关闭配置或修改当前数据不会改变历史 binlog，请重新建立全量或经过核验的增量起点", kind, p.File, ev.Header.LogPos, p.GTID)
	}
	return nil
}

type transactionEvent struct {
	Commit bool
	DDL    string
	Time   time.Time
}

// transactionState contains only binlog state, with no I/O. A caller may only
// write Changes and Position after Consume explicitly reports Commit.
type transactionState struct {
	Position    Position
	Changes     []Change
	currentGTID string
	useGTID     bool
	database    string
	tables      map[string]*TableInfo
	dameng      bool
}

func newTransactionState(cfg Config, tables []TableInfo, start Position) *transactionState {
	return &transactionState{Position: start, currentGTID: start.GTID, useGTID: start.GTID != "", database: cfg.SourceDatabase, tables: tableMap(tables), dameng: cfg.TargetDBType == "dameng"}
}
func (s *transactionState) Consume(ev *replication.BinlogEvent) (transactionEvent, error) {
	out := transactionEvent{}
	if s.dameng {
		at := s.Position
		if s.currentGTID != "" {
			at.GTID = s.currentGTID
		}
		if err := checkDamengEvent(ev, at); err != nil {
			s.Changes = nil
			return out, err
		}
	}
	if ev.Header.Timestamp > 0 {
		out.Time = time.Unix(int64(ev.Header.Timestamp), 0)
	}
	if ev.Header.LogPos > 0 {
		s.Position.Pos = ev.Header.LogPos
	}
	switch e := ev.Event.(type) {
	case *replication.RotateEvent:
		s.Position.File = string(e.NextLogName)
		s.Position.Pos = uint32(e.Position)
	case *replication.GTIDEvent:
		next := fmt.Sprintf("%s:%d", formatSID(e.SID), e.GNO)
		if next == s.currentGTID {
			s.Changes = nil
		}
		s.currentGTID = next
	case *replication.RowsEvent:
		if string(e.Table.Schema) != s.database {
			break
		}
		t := s.tables[string(e.Table.Table)]
		if t == nil {
			break
		}
		switch e.Type() {
		case replication.EnumRowsEventTypeInsert:
			for _, row := range e.Rows {
				s.Changes = append(s.Changes, Change{Action: "insert", Table: t, After: row})
			}
		case replication.EnumRowsEventTypeDelete:
			for _, row := range e.Rows {
				s.Changes = append(s.Changes, Change{Action: "delete", Table: t, Before: row})
			}
		case replication.EnumRowsEventTypeUpdate:
			if len(e.Rows)%2 != 0 {
				return out, fmt.Errorf("表 %s 的 UPDATE 行镜像不完整", t.Name)
			}
			for i := 0; i < len(e.Rows); i += 2 {
				s.Changes = append(s.Changes, Change{Action: "update", Table: t, Before: e.Rows[i], After: e.Rows[i+1]})
			}
		}
	case *replication.XIDEvent:
		if s.useGTID {
			if e.GSet != nil {
				s.Position.GTID = e.GSet.String()
			} else {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
		}
		out.Commit = true
	case *replication.QueryEvent:
		query := queryBody(string(e.Query))
		upper := strings.ToUpper(query)
		if sourceDDL(query, string(e.Schema), s.database) {
			if s.useGTID {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
			out.DDL = string(e.Query)
			return out, nil
		}
		if upper == "BEGIN" || strings.HasPrefix(upper, "SAVEPOINT") || strings.HasPrefix(upper, "XA ") {
			break
		}
		if upper == "ROLLBACK" {
			s.Changes = nil
		}
		if upper == "COMMIT" || upper == "ROLLBACK" || len(s.Changes) == 0 {
			if s.useGTID {
				s.Position.GTID = mergeGTID(s.Position.GTID, s.currentGTID)
			}
			out.Commit = true
		}
	}
	return out, nil
}
