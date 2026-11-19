package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

const postgresSequenceBoundsSQL = `SELECT n.nspname,c.relname,s.minimum_value,s.maximum_value,s.increment,s.cycle_option
	FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
	JOIN information_schema.sequences s ON s.sequence_schema=n.nspname AND s.sequence_name=c.relname
	WHERE c.oid=$1::regclass AND c.relkind='S'`

const mysqlAutoIncrementNextSQL = `SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE BINARY TABLE_SCHEMA=BINARY ? AND BINARY TABLE_NAME=BINARY ?`
const postgresSequenceColumnBoundsSQL = `SELECT data_type,numeric_precision,numeric_scale FROM information_schema.columns
	WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`

func openMySQLSequenceSource(ctx context.Context, dsn string) (*sql.Conn, func(), error) {
	db, err := OpenSource(dsn)
	if err != nil {
		return nil, nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	closeSource := func() {
		conn.Close()
		db.Close()
	}
	// MySQL 8 can cache AUTO_INCREMENT. Older versions don't have this
	// variable and read the live counter directly.
	if _, err = conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry=0"); err != nil {
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1193 {
			closeSource()
			return nil, nil, fmt.Errorf("关闭 MySQL 自增元数据缓存失败: %w", err)
		}
	}
	return conn, closeSource, nil
}

type postgresSequence struct {
	reference string
	qualified string
	label     string
	minimum   int64
	maximum   int64
	increment int64
	floor     int64
	columnMax int64
}

func postgresGeneratedColumnRange(dataType string, precision, scale sql.NullInt64) (int64, int64, error) {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "smallint", "int2":
		return math.MinInt16, math.MaxInt16, nil
	case "integer", "int4", "int":
		return math.MinInt32, math.MaxInt32, nil
	case "bigint", "int8":
		return math.MinInt64, math.MaxInt64, nil
	case "numeric", "decimal":
		if scale.Valid && scale.Int64 < 0 {
			return 0, 0, fmt.Errorf("目标自增列的小数位数为负，生成值可能被舍入")
		}
		if !precision.Valid || precision.Int64-scale.Int64 >= 19 {
			return math.MinInt64, math.MaxInt64, nil
		}
		var maximum int64
		for range max(precision.Int64-scale.Int64, 0) {
			maximum = maximum*10 + 9
		}
		return -maximum, maximum, nil
	default:
		return 0, 0, fmt.Errorf("无法确定目标自增列类型 %s 的整数范围", dataType)
	}
}

func (s postgresSequence) nextValue(ctx context.Context, db *sql.DB) (int64, error) {
	var last int64
	var called bool
	if err := db.QueryRowContext(ctx, "SELECT last_value,is_called FROM "+s.qualified).Scan(&last, &called); err != nil {
		return 0, fmt.Errorf("读取 %s 的序列水位失败: %w", s.label, err)
	}
	if last < s.minimum || last > s.maximum || (called && last > math.MaxInt64-s.increment) {
		return 0, fmt.Errorf("%s 的目标序列水位已超出可生成范围", s.label)
	}
	if called {
		last += s.increment
	}
	if last > s.maximum {
		return 0, fmt.Errorf("%s 的目标序列下一值超过最大值 %d", s.label, s.maximum)
	}
	return last, nil
}

func (a *PostgresApplier) planSequence(ctx context.Context, t *TableInfo, idx int, sourceNext int64) (postgresSequence, error) {
	var s postgresSequence
	if idx < 0 || idx >= len(t.Columns) {
		return s, fmt.Errorf("表 %s 自增列元数据无效", t.Name)
	}
	s.label = t.Name + "." + t.Columns[idx]
	ref, err := a.resolveSequence(ctx, t, idx)
	if err != nil {
		return s, err
	}
	s.reference = ref
	var schema, name, cycle string
	err = a.db.QueryRowContext(ctx, postgresSequenceBoundsSQL, ref).Scan(&schema, &name, &s.minimum, &s.maximum, &s.increment, &cycle)
	if err != nil {
		return s, fmt.Errorf("读取 %s 的序列范围失败: %w", s.label, err)
	}
	if s.increment <= 0 || s.minimum > s.maximum || cycle != "NO" {
		return s, fmt.Errorf("%s 的目标序列必须使用正步长并禁用循环", s.label)
	}
	s.qualified = qualifiedIdent(schema, name)
	next, err := s.nextValue(ctx, a.db)
	if err != nil {
		return s, err
	}
	var dataType string
	var precision, scale sql.NullInt64
	err = a.db.QueryRowContext(ctx, postgresSequenceColumnBoundsSQL, a.schema, a.name(t.Name), a.name(t.Columns[idx])).Scan(&dataType, &precision, &scale)
	if err != nil {
		return s, fmt.Errorf("读取 %s 的目标列范围失败: %w", s.label, err)
	}
	columnMin, columnMax, err := postgresGeneratedColumnRange(dataType, precision, scale)
	if err != nil {
		return s, fmt.Errorf("核对 %s 的目标列范围失败: %w", s.label, err)
	}
	s.columnMax = columnMax
	var largest sql.NullInt64
	err = a.db.QueryRowContext(ctx, fmt.Sprintf("SELECT MAX(%s) FROM %s", quoteIdent(a.name(t.Columns[idx])), a.qualified(t.Name))).Scan(&largest)
	if err != nil {
		return s, fmt.Errorf("读取 %s 的目标数据最大值失败: %w", s.label, err)
	}
	s.floor = max(next, sourceNext, columnMin)
	if largest.Valid {
		if largest.Int64 == math.MaxInt64 {
			return s, fmt.Errorf("%s 的目标最大值已超出序列可生成范围", s.label)
		}
		s.floor = max(s.floor, largest.Int64+1)
	}
	if s.floor > s.maximum {
		return s, fmt.Errorf("%s 的下一值 %d 超出目标序列最大值 %d", s.label, s.floor, s.maximum)
	}
	if s.floor > s.columnMax {
		return s, fmt.Errorf("%s 的下一值 %d 超出目标列最大值 %d", s.label, s.floor, s.columnMax)
	}
	return s, nil
}

// SyncSequences validates the full set before any setval, which postgres does
// not roll back. Source and target must remain quiescent during cutover.
func (a *PostgresApplier) SyncSequences(ctx context.Context, tables []TableInfo) error {
	var source *sql.Conn
	if a.cfg.SourceDSN != "" {
		var err error
		var closeSource func()
		source, closeSource, err = openMySQLSequenceSource(ctx, a.cfg.SourceDSN)
		if err != nil {
			return err
		}
		defer closeSource()
	}
	var advances []postgresSequence
	for i := range tables {
		t := &tables[i]
		if len(t.AutoIncrement) == 0 {
			continue
		}
		var sourceNext int64
		if source != nil {
			var raw sql.NullString
			if err := source.QueryRowContext(ctx, mysqlAutoIncrementNextSQL, a.cfg.SourceDatabase, t.Name).Scan(&raw); err != nil {
				return fmt.Errorf("读取源表 %s 的 AUTO_INCREMENT 失败: %w", t.Name, err)
			}
			var err error
			sourceNext, err = strconv.ParseInt(raw.String, 10, 64)
			if err != nil || !raw.Valid || sourceNext <= 0 {
				return fmt.Errorf("源表 %s 的 AUTO_INCREMENT %q 无效或超出目标序列可生成范围", t.Name, raw.String)
			}
		}
		for _, idx := range t.AutoIncrement {
			s, err := a.planSequence(ctx, t, idx, sourceNext)
			if err != nil {
				return err
			}
			advances = append(advances, s)
		}
	}
	for _, s := range advances {
		// Another planned column may use this same sequence.
		current, err := s.nextValue(ctx, a.db)
		if err != nil {
			return err
		}
		if current > s.columnMax {
			return fmt.Errorf("%s 的下一值 %d 超出目标列最大值 %d", s.label, current, s.columnMax)
		}
		if s.floor <= current {
			continue
		}
		if _, err = a.db.ExecContext(ctx, "SELECT setval($1::regclass,$2,false)", s.reference, s.floor); err != nil {
			return fmt.Errorf("校正 %s 的自增序列 %s 失败: %w", s.label, s.reference, err)
		}
	}
	return nil
}
