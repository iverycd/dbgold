package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbgold/datamigrate/failure"
)

// OracleSQLServerReader shares the raw Oracle catalog and exact-value cursor
// with the Oracle→达梦 reader. SQL Server makes its own generator decisions.
type OracleSQLServerReader struct{ *OracleDamengReader }

func NewOracleForSQLServer(dsn, owner string, pool ConnPoolConfig) (*OracleSQLServerReader, error) {
	r, err := NewOracle(dsn, owner, pool)
	if err != nil {
		return nil, err
	}
	r.owner = owner // A catalog-selected quoted owner keeps its original spelling.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if r.owner == "" {
		err = r.db.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL`).Scan(&r.owner)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("读取 Oracle 当前 schema: %w", err)
		}
	}
	if r.version < 12 {
		// Catalog access can be sufficient for identities when V$INSTANCE is
		// unavailable to the migration account.
		var count int
		err = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_TAB_IDENTITY_COLS WHERE 1=0`).Scan(&count)
		if err == nil {
			r.version = 12
		} else if failure.IsTaskError(err) {
			r.Close()
			return nil, err
		}
	}
	return WrapOracleForSQLServer(r), nil
}

func WrapOracleForSQLServer(r *OracleReader) *OracleSQLServerReader {
	reader := WrapOracleForDameng(r)
	reader.readDefaultOnNull = r.version >= 12
	return &OracleSQLServerReader{OracleDamengReader: reader}
}

func (r *OracleSQLServerReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	return r.readRawOracleViews(ctx, true)
}

func (r *OracleSQLServerReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	return r.GetViews(ctx)
}

func (r *OracleSQLServerReader) GetTriggers(ctx context.Context) ([]TriggerInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT TRIGGER_NAME,TABLE_NAME,TABLE_OWNER,TRIGGER_TYPE,TRIGGERING_EVENT,TRIGGER_BODY,OWNER
 FROM ALL_TRIGGERS WHERE TABLE_OWNER=:1 AND TABLE_NAME IS NOT NULL ORDER BY TABLE_NAME,OWNER,TRIGGER_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []TriggerInfo
	for rows.Next() {
		var trigger TriggerInfo
		var body sql.NullString
		if err := rows.Scan(&trigger.Name, &trigger.TableName, &trigger.Schema, &trigger.Timing, &trigger.Event, &body, &trigger.Definer); err != nil {
			return nil, err
		}
		trigger.Body = body.String
		if strings.TrimSpace(trigger.Body) == "" {
			trigger.MetadataError = "源库未返回触发器正文"
		}
		triggers = append(triggers, trigger)
	}
	return triggers, rows.Err()
}

func (r *OracleSQLServerReader) GetPrimaryKeys(ctx context.Context) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.CONSTRAINT_NAME,cc.COLUMN_NAME,c.STATUS,c.VALIDATED,c.DEFERRABLE,c.DEFERRED
 FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc
 ON c.OWNER=cc.OWNER AND c.CONSTRAINT_NAME=cc.CONSTRAINT_NAME AND c.TABLE_NAME=cc.TABLE_NAME
 WHERE c.OWNER=:1 AND c.CONSTRAINT_TYPE='P' ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []IndexInfo
	for rows.Next() {
		var table, name, column, status, validated, deferrable, deferred string
		if err := rows.Scan(&table, &name, &column, &status, &validated, &deferrable, &deferred); err != nil {
			return nil, err
		}
		if len(keys) == 0 || keys[len(keys)-1].TableName != table || keys[len(keys)-1].IndexName != name {
			key := IndexInfo{TableName: table, IndexName: name, IsPrimary: true, IsUnique: true}
			if status != "ENABLED" || validated != "VALIDATED" || deferrable != "NOT DEFERRABLE" || deferred != "IMMEDIATE" {
				key.Unsupported = "禁用、未验证或可延迟主键暂不支持等价迁移"
			}
			keys = append(keys, key)
		}
		keys[len(keys)-1].Columns = append(keys[len(keys)-1].Columns, column)
	}
	return keys, rows.Err()
}

func (r *OracleSQLServerReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	return r.SQLServerSequences(ctx, nil)
}

func (r *OracleSQLServerReader) SQLServerSequences(ctx context.Context, tables []string) ([]SequenceInfo, error) {
	return r.oracleSequences(ctx, true, sqlServerTableSet(tables))
}

func (r *OracleDamengReader) oracleSQLServerIndependentSequences(ctx context.Context, bound map[string]bool) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT SEQUENCE_NAME,TO_CHAR(LAST_NUMBER,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),
 TO_CHAR(INCREMENT_BY,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(MIN_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),
 TO_CHAR(MAX_VALUE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),TO_CHAR(CACHE_SIZE,'TM9','NLS_NUMERIC_CHARACTERS=''.,'''),CYCLE_FLAG,ORDER_FLAG
 FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=:1 ORDER BY SEQUENCE_NAME`, r.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SequenceInfo
	for rows.Next() {
		seq := SequenceInfo{Native: &NativeSequenceInfo{Schema: r.owner}}
		var cycle, order string
		if err := rows.Scan(&seq.Native.Name, &seq.StartValueText, &seq.Native.IncrementBy, &seq.Native.MinValue,
			&seq.Native.MaxValue, &seq.Native.CacheSize, &cycle, &order); err != nil {
			return nil, err
		}
		if !bound[r.owner+"\x00"+seq.Native.Name] {
			seq.Native.Cycle, seq.Native.Ordered = cycle == "Y", order == "Y"
			out = append(out, seq)
		}
	}
	return out, rows.Err()
}

func (r *OracleSQLServerReader) SQLServerDecimalProfile(ctx context.Context, table string, columns []string, consume func(string, interface{}) error) error {
	return sqlServerDecimalProfile(ctx, r.db, r.tableName(table), columns, func(name string) string {
		return "TO_CHAR(" + quoteOracleIdentifier(name) + ", 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''')"
	}, consume)
}

var _ Reader = (*OracleSQLServerReader)(nil)
