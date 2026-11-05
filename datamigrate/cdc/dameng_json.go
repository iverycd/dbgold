package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
)

// go-mysql serializes binary JSON opaque DECIMAL as a JSON string, losing its
// numeric type. Inspect the original bytes before its JSON decoder runs. JSON
// and BLOB share the same length prefix, so a private table-map copy lets the
// existing row decoder expose those bytes without duplicating row decoding or
// mutating the shared table map. Unsupported values stop before any checkpoint.
func decodeDamengRowsEvent(event *replication.RowsEvent, data []byte) error {
	pos, err := event.DecodeHeader(data)
	if err != nil {
		return err
	}
	if event.Type() == replication.EnumRowsEventTypeUnknown {
		return fmt.Errorf("达梦 CDC 不支持 JSON 部分更新事件，请禁用 binlog_row_value_options=PARTIAL_JSON 并重新建立全量起点；原检查点保留")
	}
	var columns []int
	for i, typ := range event.Table.ColumnType {
		if typ == gomysql.MYSQL_TYPE_JSON {
			columns = append(columns, i)
		}
	}
	if len(columns) == 0 {
		return decodeDamengFullRows(event, pos, data)
	}
	original := event.Table
	copyTable := *original
	copyTable.ColumnType = append([]byte(nil), original.ColumnType...)
	for _, column := range columns {
		copyTable.ColumnType[column] = gomysql.MYSQL_TYPE_BLOB
	}
	event.Table = &copyTable
	err = decodeDamengFullRows(event, pos, data)
	event.Table = original
	if err != nil {
		return err
	}
	type opaqueCell struct {
		row, column int
		values      []damengJSONOpaque
	}
	var opaqueCells []opaqueCell
	for rowIndex, row := range event.Rows {
		for _, column := range columns {
			if row[column] == nil {
				continue
			}
			raw, ok := row[column].([]byte)
			if !ok || len(raw) == 0 {
				return fmt.Errorf("表 %s JSON binary 值为空或无法识别", original.Table)
			}
			values, inspectErr := collectDamengJSONOpaque(raw[0], raw[1:], 0, nil)
			if inspectErr != nil {
				return fmt.Errorf("表 %s JSON 列 %d: %w", original.Table, column+1, inspectErr)
			}
			if len(values) > 0 {
				opaqueCells = append(opaqueCells, opaqueCell{rowIndex, column, values})
			}
		}
	}
	if err = decodeDamengFullRows(event, pos, data); err != nil {
		return err
	}
	for _, cell := range opaqueCells {
		text, ok := event.Rows[cell.row][cell.column].(string)
		if !ok {
			return fmt.Errorf("JSON 解码值类型无效")
		}
		doc, buildErr := buildDamengJSONDocument(text, cell.values)
		if buildErr != nil {
			return buildErr
		}
		event.Rows[cell.row][cell.column] = doc
	}
	return nil
}

func decodeDamengFullRows(event *replication.RowsEvent, pos int, data []byte) error {
	if err := event.DecodeData(pos, data); err != nil {
		return err
	}
	for _, skipped := range event.SkippedColumns {
		if len(skipped) > 0 {
			return fmt.Errorf("达梦 CDC 需要 binlog_row_image=FULL，收到部分行镜像；原检查点保留")
		}
	}
	return nil
}

func checkDamengBinaryJSON(typ byte, data []byte, depth int) error {
	_, err := collectDamengJSONOpaque(typ, data, depth, nil)
	return err
}

const damengJSONBatchSize = 128
const damengJSONBatchBytes = 4 << 20
const damengSourceJSONExpression = `CAST(? AS JSON)`

// MySQL's SQL JSON text has its own whitespace, key order and number rendering.
// Full migration stores that text in CLOB. Re-render both row images on MySQL
// before equality matching, so a pre-existing snapshot row can be located.
func normalizeDamengJSON(ctx context.Context, source *sql.DB, changes []Change) ([]Change, error) {
	type cell struct {
		change, column int
		before         bool
		json           string
	}
	result := append([]Change(nil), changes...)
	var cells []cell
	var documents []damengJSONDocument
	seen := map[string]bool{}
	for i, change := range result {
		if change.Table == nil || len(change.Table.Columns) != len(change.Table.ColumnTypes) {
			return nil, fmt.Errorf("CDC 变更缺少完整源表元数据")
		}
		for column, typ := range change.Table.ColumnTypes {
			if !strings.EqualFold(typ, "JSON") {
				continue
			}
			for _, before := range []bool{true, false} {
				row := change.After
				if before {
					row = change.Before
				}
				if row == nil {
					continue
				}
				if column >= len(row) {
					return nil, fmt.Errorf("JSON 列超出 binlog 行范围")
				}
				if row[column] == nil {
					continue
				}
				var document damengJSONDocument
				switch value := row[column].(type) {
				case string:
					document.Text = value
				case []byte:
					document.Text = string(value)
				case damengJSONDocument:
					document = value
				case *replication.JsonDiff:
					return nil, fmt.Errorf("达梦 CDC 不支持 JSON 部分更新，请禁用 binlog_row_value_options=PARTIAL_JSON；原检查点保留")
				default:
					return nil, fmt.Errorf("JSON binlog 值类型不支持: %T", row[column])
				}
				key := document.key()
				cells = append(cells, cell{i, column, before, key})
				if !seen[key] {
					seen[key] = true
					documents = append(documents, document)
				}
			}
		}
	}
	if len(cells) == 0 {
		return result, nil
	}
	canonical := make(map[string]string, len(documents))
	for start := 0; start < len(documents); {
		end, bytes := start, 0
		for end < len(documents) && end-start < damengJSONBatchSize {
			if end > start && bytes+len(documents[end].Text) > damengJSONBatchBytes {
				break
			}
			bytes += len(documents[end].Text)
			end++
		}
		batch := documents[start:end]
		marks, dest := make([]string, len(batch)), make([]any, len(batch))
		var args []any
		values := make([]sql.NullString, len(batch))
		for i, document := range batch {
			query, parameters := document.query()
			marks[i] = query
			args = append(args, parameters...)
			dest[i] = &values[i]
		}
		if err := source.QueryRowContext(ctx, "SELECT "+strings.Join(marks, ","), args...).Scan(dest...); err != nil {
			return nil, fmt.Errorf("按源库规范转换 JSON 失败: %w", err)
		}
		for i, value := range values {
			if !value.Valid {
				return nil, fmt.Errorf("源库返回空 JSON 规范值")
			}
			canonical[batch[i].key()] = value.String
		}
		start = end
	}
	type rowKey struct {
		change int
		before bool
	}
	copied := map[rowKey]bool{}
	for _, cell := range cells {
		key := rowKey{cell.change, cell.before}
		row := &result[cell.change].After
		if cell.before {
			row = &result[cell.change].Before
		}
		if !copied[key] {
			*row = append([]any(nil), (*row)...)
			copied[key] = true
		}
		(*row)[cell.column] = canonical[cell.json]
	}
	return result, nil
}
