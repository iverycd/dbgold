package source

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/sqlsyntax"
	"fmt"
	"math/big"
	"strings"
)

func crossIdentity(table, column, schema, seed, step string, last sql.NullString, columnType ColumnInfo, product string) SequenceInfo {
	s := SequenceInfo{TableName: table, ColumnName: column, Native: &NativeSequenceInfo{Schema: schema, Name: "SEQ_" + table + "_" + column, IncrementBy: step, MinValue: "-9223372036854775808", MaxValue: "9223372036854775807", CacheSize: "0", Bindings: []SequenceBinding{{Schema: schema, TableName: table, ColumnName: column, Kind: "identity", Identity: product + "_identity"}}}}
	s.Native.SeedValue = seed
	min, max, err := crossIdentityBounds(columnType, product)
	if err != nil {
		s.Unsupported = err.Error()
	} else {
		s.Native.MinValue, s.Native.MaxValue = min, max
	}
	s.StartValueText, err = crossNext(seed, step, last)
	if err != nil {
		s.Unsupported = err.Error()
	}
	return s
}
func (r *SQLServerOracleReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.name,c.name,CONVERT(varchar(100),c.seed_value),CONVERT(varchar(100),c.increment_value),CONVERT(varchar(100),c.last_value),TYPE_NAME(c.system_type_id),c.precision
 FROM sys.tables t JOIN sys.identity_columns c ON c.object_id=t.object_id WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 ORDER BY t.name,c.column_id`)
	if err != nil {
		return nil, err
	}
	var out []SequenceInfo
	for rows.Next() {
		var table, col, seed, step string
		var last sql.NullString
		var ct ColumnInfo
		if err = rows.Scan(&table, &col, &seed, &step, &last, &ct.DataType, &ct.Precision); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, crossIdentity(table, col, "dbo", seed, step, last, ct, "sqlserver"))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT q.name,CONVERT(varchar(100),q.start_value),CONVERT(varchar(100),q.increment),CONVERT(varchar(100),q.current_value),CONVERT(varchar(100),q.minimum_value),CONVERT(varchar(100),q.maximum_value),q.is_cycling,q.is_exhausted,COALESCE(CONVERT(varchar(100),q.cache_size),'0') FROM sys.sequences q WHERE q.schema_id=SCHEMA_ID('dbo') ORDER BY q.name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s SequenceInfo
		var seed, step string
		var current sql.NullString
		var exhausted bool
		s.Native = &NativeSequenceInfo{Schema: "dbo"}
		if err = rows.Scan(&s.Native.Name, &seed, &step, &current, &s.Native.MinValue, &s.Native.MaxValue, &s.Native.Cycle, &exhausted, &s.Native.CacheSize); err != nil {
			rows.Close()
			return nil, err
		}
		s.Native.IncrementBy = step
		s.StartValueText, err = crossNext(seed, step, current)
		if err != nil {
			s.Unsupported = err.Error()
		}
		if exhausted {
			s.Unsupported = "源序列已耗尽"
		}
		out = append(out, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT s.name,SCHEMA_NAME(t.schema_id),t.name,c.name,dc.definition FROM sys.sql_expression_dependencies d JOIN sys.sequences s ON s.object_id=d.referenced_id JOIN sys.default_constraints dc ON dc.object_id=d.referencing_id JOIN sys.tables t ON t.object_id=dc.parent_object_id JOIN sys.columns c ON c.object_id=t.object_id AND c.column_id=dc.parent_column_id WHERE s.schema_id=SCHEMA_ID('dbo') ORDER BY s.name,t.name,c.column_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, schema, table, col, def string
		if err = rows.Scan(&seq, &schema, &table, &col, &def); err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].TableName == "" && out[i].Native.Name == seq {
				if schema != "dbo" || !directCrossSequenceDefault(def, "sqlserver", "dbo", seq) {
					out[i].Unsupported = "序列存在跨 Schema 或非直接默认值绑定，需要人工适配"
				}
				out[i].Native.Bindings = append(out[i].Native.Bindings, SequenceBinding{Schema: schema, TableName: table, ColumnName: col, Kind: "default", Default: def})
			}
		}
	}
	return out, rows.Err()
}

func (r *DaMengCrossReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	// Read only generator/default catalogs here. A failed table descriptor must
	// not turn an unrelated generator into a task-wide metadata failure.
	rows, err := r.db.QueryContext(ctx, `SELECT b.TABLE_NAME,a.NAME,c.DATA_TYPE,COALESCE(c.DATA_PRECISION,0)
 FROM ALL_TABLES b JOIN ALL_OBJECTS o ON o.OWNER=b.OWNER AND o.OBJECT_NAME=b.TABLE_NAME AND o.OBJECT_TYPE='TABLE'
 JOIN SYS.SYSCOLUMNS a ON a.ID=o.OBJECT_ID JOIN ALL_TAB_COLUMNS c ON c.OWNER=b.OWNER AND c.TABLE_NAME=b.TABLE_NAME AND c.COLUMN_NAME=a.NAME
 WHERE b.OWNER=? AND a.INFO2 & 0x01 = 0x01 ORDER BY b.TABLE_NAME,a.COLID`, r.schema)
	if err != nil {
		return nil, err
	}
	type identityColumn struct {
		table, col string
		typ        ColumnInfo
	}
	var ids []identityColumn
	for rows.Next() {
		var c identityColumn
		if err = rows.Scan(&c.table, &c.col, &c.typ.DataType, &c.typ.Precision); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []SequenceInfo
	for _, c := range ids {
		q := crossQuote(r.schema) + "." + crossQuote(c.table)
		var seed, step string
		var last sql.NullString
		e := r.db.QueryRowContext(ctx, `SELECT CAST(IDENT_SEED(?) AS VARCHAR(100)),CAST(IDENT_INCR(?) AS VARCHAR(100)),CAST(IDENT_CURRENT(?) AS VARCHAR(100))`, q, q, q).Scan(&seed, &step, &last)
		if failure.IsTaskError(e) {
			return nil, e
		}
		seq := crossIdentity(c.table, c.col, r.schema, seed, step, last, c.typ, "dameng")
		if e != nil {
			seq.Unsupported = e.Error()
		}
		out = append(out, seq)
	}
	rows, err = r.db.QueryContext(ctx, `SELECT OWNER,TABLE_NAME,COLUMN_NAME,DATA_DEFAULT FROM ALL_TAB_COLUMNS ORDER BY OWNER,TABLE_NAME,COLUMN_ID`)
	if err != nil {
		return nil, err
	}
	var bindings []SequenceBinding
	for rows.Next() {
		var b SequenceBinding
		b.Kind = "default"
		var def sql.NullString
		if err = rows.Scan(&b.Schema, &b.TableName, &b.ColumnName, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if !def.Valid {
			continue
		}
		b.Default = def.String
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT SEQUENCE_NAME,CAST(LAST_NUMBER AS VARCHAR(100)),CAST(INCREMENT_BY AS VARCHAR(100)),CAST(MIN_VALUE AS VARCHAR(100)),CAST(MAX_VALUE AS VARCHAR(100)),CAST(CACHE_SIZE AS VARCHAR(100)),CYCLE_FLAG,ORDER_FLAG FROM ALL_SEQUENCES WHERE SEQUENCE_OWNER=? ORDER BY SEQUENCE_NAME`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := SequenceInfo{Native: &NativeSequenceInfo{Schema: r.schema}}
		var cycle, order string
		if err = rows.Scan(&s.Native.Name, &s.StartValueText, &s.Native.IncrementBy, &s.Native.MinValue, &s.Native.MaxValue, &s.Native.CacheSize, &cycle, &order); err != nil {
			return nil, err
		}
		s.Native.Cycle = cycle == "Y"
		s.Native.Ordered = order == "Y"
		for _, b := range bindings {
			if directCrossSequenceDefault(b.Default, "dameng", r.schema, s.Native.Name) {
				if b.Schema != r.schema {
					tokens, e := sqlsyntax.Lex(b.Default)
					if e != nil {
						continue
					}
					for len(tokens) > 2 && tokens[0].Is("(") && tokens[len(tokens)-1].Is(")") {
						tokens = tokens[1 : len(tokens)-1]
					}
					if len(tokens) != 5 {
						continue
					}
					s.Unsupported = "序列存在跨 Schema 默认值绑定，需要人工适配"
				}
				s.Native.Bindings = append(s.Native.Bindings, b)
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (r *DaMengCrossReader) SQLServerSequences(ctx context.Context, _ []string) ([]SequenceInfo, error) {
	return r.GetSequences(ctx)
}
func (r *DaMengCrossReader) SQLServerDecimalProfile(ctx context.Context, table string, cols []string, consume func(string, any) error) error {
	return sqlServerDecimalProfile(ctx, r.db, crossQuote(r.schema)+"."+crossQuote(table), cols, func(c string) string { return "CAST(" + crossQuote(c) + " AS VARCHAR(256))" }, consume)
}

// Named cross-schema references are retained and rejected, never rebound to a
// same-name object in the selected target schema.
func (r *DaMengCrossReader) GetForeignKeys(ctx context.Context) ([]FKInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.TABLE_NAME,c.CONSTRAINT_NAME,cc.COLUMN_NAME,rc.OWNER,rc.TABLE_NAME,rcc.COLUMN_NAME,c.DELETE_RULE,c.STATUS,c.DEFERRABLE FROM ALL_CONSTRAINTS c JOIN ALL_CONS_COLUMNS cc ON cc.OWNER=c.OWNER AND cc.CONSTRAINT_NAME=c.CONSTRAINT_NAME JOIN ALL_CONSTRAINTS rc ON rc.OWNER=c.R_OWNER AND rc.CONSTRAINT_NAME=c.R_CONSTRAINT_NAME JOIN ALL_CONS_COLUMNS rcc ON rcc.OWNER=rc.OWNER AND rcc.CONSTRAINT_NAME=rc.CONSTRAINT_NAME AND rcc.POSITION=cc.POSITION WHERE c.OWNER=? AND c.CONSTRAINT_TYPE='R' ORDER BY c.TABLE_NAME,c.CONSTRAINT_NAME,cc.POSITION`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FKInfo
	for rows.Next() {
		var f FKInfo
		var col, ref, status, deferred string
		if err = rows.Scan(&f.TableName, &f.ConstraintName, &col, &f.RefSchema, &f.RefTable, &ref, &f.OnDelete, &status, &deferred); err != nil {
			return nil, err
		}
		f.OnUpdate = "NO ACTION"
		if f.RefSchema != r.schema || status != "ENABLED" || deferred != "NOT DEFERRABLE" {
			f.Unsupported = "跨 Schema、禁用或可延期外键需要人工适配"
		}
		if len(out) == 0 || out[len(out)-1].TableName != f.TableName || out[len(out)-1].ConstraintName != f.ConstraintName {
			out = append(out, f)
		}
		p := &out[len(out)-1]
		p.Columns = append(p.Columns, col)
		p.RefColumns = append(p.RefColumns, ref)
	}
	return out, rows.Err()
}

func crossIdentityBounds(c ColumnInfo, product string) (string, string, error) {
	switch strings.ToLower(c.DataType) {
	case "tinyint", "byte":
		if product == "sqlserver" {
			return "0", "255", nil
		}
		return "-128", "127", nil
	case "smallint":
		return "-32768", "32767", nil
	case "int", "integer":
		return "-2147483648", "2147483647", nil
	case "bigint":
		return "-9223372036854775808", "9223372036854775807", nil
	case "number", "decimal", "numeric", "dec":
		if c.Precision > 0 && c.Precision <= 38 && c.Scale == 0 {
			v := new(big.Int).Exp(big.NewInt(10), big.NewInt(c.Precision), nil)
			v.Sub(v, big.NewInt(1))
			return "-" + v.String(), v.String(), nil
		}
	}
	return "", "", fmt.Errorf("无法确认自增列 %s 的整数边界", c.DataType)
}
func directCrossSequenceDefault(raw, product, schema, name string) bool {
	var ts []sqlsyntax.Token
	var err error
	if product == "sqlserver" {
		ts, err = sqlsyntax.LexTSQL(raw)
	} else {
		ts, err = sqlsyntax.Lex(raw)
	}
	if err != nil {
		return false
	}
	for len(ts) > 2 && ts[0].Is("(") && ts[len(ts)-1].Is(")") {
		ts = ts[1 : len(ts)-1]
	}
	if product == "sqlserver" {
		if len(ts) < 4 || !ts[0].Is("NEXT") || !ts[1].Is("VALUE") || !ts[2].Is("FOR") {
			return false
		}
		ts = ts[3:]
	} else {
		if len(ts) < 3 || !ts[len(ts)-2].Is(".") || !ts[len(ts)-1].Is("NEXTVAL") {
			return false
		}
		ts = ts[:len(ts)-2]
	}
	match := func(t sqlsyntax.Token, want string) bool {
		if t.Kind == 'q' {
			return t.Value == want
		}
		if t.Kind != 'i' {
			return false
		}
		if product == "dameng" {
			return strings.ToUpper(t.Text) == want
		}
		return strings.EqualFold(t.Text, want)
	}
	return len(ts) == 1 && match(ts[0], name) || len(ts) == 3 && match(ts[0], schema) && ts[1].Is(".") && match(ts[2], name)
}
