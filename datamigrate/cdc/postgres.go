package cdc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/valueconv"
	_ "github.com/lib/pq"
)

type PostgresApplier struct {
	db     *sql.DB
	schema string
	jobID  string
	lower  bool
	conv   *valueconv.PostgresValueConverter
}

func NewPostgresApplier(ctx context.Context, dsn, schema, jobID string, lower bool) (*PostgresApplier, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	a := &PostgresApplier{db: db, schema: schema, jobID: jobID, lower: lower, conv: valueconv.NewPostgres()}
	if err = a.ensureCheckpoint(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *PostgresApplier) Close() error { return a.db.Close() }

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (a *PostgresApplier) name(s string) string {
	if a.lower {
		return strings.ToLower(s)
	}
	return s
}
func (a *PostgresApplier) qualified(table string) string {
	return quoteIdent(a.schema) + "." + quoteIdent(a.name(table))
}
func (a *PostgresApplier) checkpointTable() string {
	return quoteIdent(a.schema) + `."__dbgold_cdc_checkpoint"`
}

func (a *PostgresApplier) ensureCheckpoint(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		job_id text PRIMARY KEY, binlog_file text NOT NULL DEFAULT '', binlog_position bigint NOT NULL DEFAULT 4,
		gtid text NOT NULL DEFAULT '', updated_at timestamptz NOT NULL DEFAULT now())`, a.checkpointTable()))
	return err
}

func (a *PostgresApplier) LoadCheckpoint(ctx context.Context) (Position, bool, error) {
	var p Position
	var n int64
	err := a.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT binlog_file, binlog_position, gtid FROM %s WHERE job_id=$1`, a.checkpointTable()), a.jobID).Scan(&p.File, &n, &p.GTID)
	if err == sql.ErrNoRows {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, err
	}
	p.Pos = uint32(n)
	return p, true, nil
}

func (a *PostgresApplier) SaveCheckpoint(ctx context.Context, p Position) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = a.saveCheckpointTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *PostgresApplier) saveCheckpointTx(ctx context.Context, tx *sql.Tx, p Position) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(job_id,binlog_file,binlog_position,gtid,updated_at)
		VALUES($1,$2,$3,$4,now()) ON CONFLICT(job_id) DO UPDATE SET binlog_file=EXCLUDED.binlog_file,
		binlog_position=EXCLUDED.binlog_position,gtid=EXCLUDED.gtid,updated_at=now()`, a.checkpointTable()), a.jobID, p.File, p.Pos, p.GTID)
	return err
}

func (a *PostgresApplier) Apply(ctx context.Context, changes []Change, p Position) (Stats, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback()
	var stats Stats
	for _, ch := range changes {
		switch ch.Action {
		case "insert":
			if err = a.insert(ctx, tx, ch.Table, ch.After); err == nil {
				stats.Inserts++
			}
		case "update":
			if len(ch.Table.PrimaryKey) == 0 {
				stats.Skipped++
				stats.Warnings++
				continue
			}
			if primaryKeyChanged(ch.Table, ch.Before, ch.After) {
				if err = a.delete(ctx, tx, ch.Table, ch.Before); err != nil {
					break
				}
			}
			if err = a.insert(ctx, tx, ch.Table, ch.After); err == nil {
				stats.Updates++
			}
		case "delete":
			if len(ch.Table.PrimaryKey) == 0 {
				stats.Skipped++
				stats.Warnings++
				continue
			}
			if err = a.delete(ctx, tx, ch.Table, ch.Before); err == nil {
				stats.Deletes++
			}
		}
		if err != nil {
			return Stats{}, fmt.Errorf("apply %s %s: %w", ch.Action, ch.Table.Name, err)
		}
	}
	if err = a.saveCheckpointTx(ctx, tx, p); err != nil {
		return Stats{}, err
	}
	if err = tx.Commit(); err != nil {
		return Stats{}, err
	}
	stats.Position, stats.LastEventAt = p, time.Now()
	return stats, nil
}

func primaryKeyChanged(t *TableInfo, before, after []any) bool {
	for _, i := range t.PrimaryKey {
		if i >= len(before) || i >= len(after) || fmt.Sprint(before[i]) != fmt.Sprint(after[i]) {
			return true
		}
	}
	return false
}

func (a *PostgresApplier) insert(ctx context.Context, tx *sql.Tx, t *TableInfo, row []any) error {
	if len(row) != len(t.Columns) {
		return fmt.Errorf("列数不匹配: event=%d metadata=%d", len(row), len(t.Columns))
	}
	cols := make([]string, len(t.Columns))
	marks := make([]string, len(t.Columns))
	vals := make([]any, len(row))
	for i, c := range t.Columns {
		cols[i] = quoteIdent(a.name(c))
		marks[i] = fmt.Sprintf("$%d", i+1)
		vals[i] = a.conv.Convert(row[i], "mysql", t.ColumnTypes[i])
	}
	sqlText := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", a.qualified(t.Name), strings.Join(cols, ","), strings.Join(marks, ","))
	if len(t.PrimaryKey) > 0 {
		pk := make([]string, len(t.PrimaryKey))
		set := make([]string, 0, len(t.Columns))
		pkSet := map[int]bool{}
		for i, p := range t.PrimaryKey {
			pk[i] = quoteIdent(a.name(t.Columns[p]))
			pkSet[p] = true
		}
		for i, c := range t.Columns {
			if !pkSet[i] {
				q := quoteIdent(a.name(c))
				set = append(set, q+"=EXCLUDED."+q)
			}
		}
		if len(set) == 0 {
			sqlText += " ON CONFLICT (" + strings.Join(pk, ",") + ") DO NOTHING"
		} else {
			sqlText += " ON CONFLICT (" + strings.Join(pk, ",") + ") DO UPDATE SET " + strings.Join(set, ",")
		}
	}
	_, err := tx.ExecContext(ctx, sqlText, vals...)
	return err
}

func (a *PostgresApplier) delete(ctx context.Context, tx *sql.Tx, t *TableInfo, row []any) error {
	where := make([]string, len(t.PrimaryKey))
	vals := make([]any, len(t.PrimaryKey))
	for i, p := range t.PrimaryKey {
		where[i] = quoteIdent(a.name(t.Columns[p])) + fmt.Sprintf("=$%d", i+1)
		vals[i] = a.conv.Convert(row[p], "mysql", t.ColumnTypes[p])
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM "+a.qualified(t.Name)+" WHERE "+strings.Join(where, " AND "), vals...)
	return err
}

// SyncSequences moves PostgreSQL sequences to at least MAX(column)+1.
func (a *PostgresApplier) SyncSequences(ctx context.Context, tables []TableInfo) error {
	for _, t := range tables {
		for _, idx := range t.AutoIncrement {
			var seq sql.NullString
			err := a.db.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, a.schema+"."+a.name(t.Name), a.name(t.Columns[idx])).Scan(&seq)
			if err != nil || !seq.Valid {
				candidate := a.schema + ".seq_" + a.name(t.Name) + "_" + a.name(t.Columns[idx])
				if e := a.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, candidate).Scan(&seq); e != nil || !seq.Valid {
					continue
				}
			}
			_, err = a.db.ExecContext(ctx, fmt.Sprintf(`SELECT setval($1::regclass, COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)`, quoteIdent(a.name(t.Columns[idx])), a.qualified(t.Name)), seq.String)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
