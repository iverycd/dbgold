package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate/valueconv"
)

const damengTimestampBatchSize = 128
const damengSourceTimestampExpression = `DATE_FORMAT(FROM_UNIXTIME(CAST(? AS DECIMAL(20,6))), '%Y-%m-%d %H:%i:%s.%f')`

// The full MySQL reader sees TIMESTAMP in the source connection's session
// time zone. Resolve UTC binlog timestamps on that same DSN, rather than
// guessing from ambiguous SYSTEM/CST zone names or today's UTC offset. MySQL
// applies its historical timezone rules, including DST, for every instant.
func normalizeDamengTimestamps(ctx context.Context, source *sql.DB, changes []Change) ([]Change, error) {
	type cell struct {
		change, column int
		before         bool
		epoch          string
	}
	result := append([]Change(nil), changes...)
	var cells []cell
	var epochs []string
	seen := map[string]bool{}
	for changeIndex, change := range result {
		if change.Table == nil || len(change.Table.Columns) != len(change.Table.ColumnTypes) {
			return nil, fmt.Errorf("CDC 变更缺少完整源表元数据")
		}
		for column, typ := range change.Table.ColumnTypes {
			if !strings.EqualFold(typ, "TIMESTAMP") {
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
					return nil, fmt.Errorf("表 %s TIMESTAMP 列超出 binlog 行范围", change.Table.Name)
				}
				epoch, convert, err := damengTimestampEpoch(row[column])
				if err != nil {
					return nil, fmt.Errorf("表 %s TIMESTAMP 列 %s: %w", change.Table.Name, change.Table.Columns[column], err)
				}
				if !convert {
					continue
				}
				cells = append(cells, cell{change: changeIndex, column: column, before: before, epoch: epoch})
				if !seen[epoch] {
					seen[epoch] = true
					epochs = append(epochs, epoch)
				}
			}
		}
	}
	if len(cells) == 0 {
		return result, nil
	}
	converted := make(map[string]string, len(epochs))
	for start := 0; start < len(epochs); start += damengTimestampBatchSize {
		batch := epochs[start:min(start+damengTimestampBatchSize, len(epochs))]
		marks, args, dest := make([]string, len(batch)), make([]any, len(batch)), make([]any, len(batch))
		values := make([]sql.NullString, len(batch))
		for i, epoch := range batch {
			marks[i] = damengSourceTimestampExpression
			args[i] = epoch
			dest[i] = &values[i]
		}
		if err := source.QueryRowContext(ctx, "SELECT "+strings.Join(marks, ","), args...).Scan(dest...); err != nil {
			return nil, fmt.Errorf("按源会话时区转换 TIMESTAMP 失败: %w", err)
		}
		for i, value := range values {
			if !value.Valid {
				return nil, fmt.Errorf("源会话无法表示 TIMESTAMP Unix 时间 %s", batch[i])
			}
			if _, err := time.Parse("2006-01-02 15:04:05.999999", value.String); err != nil {
				return nil, fmt.Errorf("源会话返回无效 TIMESTAMP: %w", err)
			}
			converted[batch[i]] = value.String
		}
	}
	// Clone only touched before/after rows. Do not alter replication-owned
	// buffers or change DATETIME fields, which already carry wall-clock values.
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
		(*row)[cell.column] = converted[cell.epoch]
	}
	return result, nil
}

func damengTimestampEpoch(value any) (string, bool, error) {
	if value == nil {
		return "", false, nil
	}
	var instant time.Time
	switch v := value.(type) {
	case string:
		if valueconv.MySQLDamengZeroDate(v) {
			return "", false, nil
		}
		var err error
		instant, err = time.Parse("2006-01-02 15:04:05.999999", v)
		if err != nil {
			return "", false, err
		}
	case []byte:
		return damengTimestampEpoch(string(v))
	case time.Time:
		instant = v.UTC()
	default:
		return "", false, fmt.Errorf("binlog TIMESTAMP 值类型不支持: %T", value)
	}
	if instant.Nanosecond()%1000 != 0 {
		return "", false, fmt.Errorf("TIMESTAMP 超过微秒精度")
	}
	return strconv.FormatInt(instant.Unix(), 10) + fmt.Sprintf(".%06d", instant.Nanosecond()/1000), true, nil
}
