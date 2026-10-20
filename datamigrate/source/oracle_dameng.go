package source

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// OracleDamengReader keeps Oracle metadata and values out of the historical
// PostgreSQL conversion path. Source identifiers are never case-normalized.
type OracleDamengReader struct {
	*OracleReader
	metadataMu sync.RWMutex
	metadata   map[string]*TableDDLInfo
}

func WrapOracleForDameng(r *OracleReader) *OracleDamengReader {
	if r.version == 0 {
		r.version = 11
	}
	return &OracleDamengReader{OracleReader: r, metadata: map[string]*TableDDLInfo{}}
}

func NewOracleForDameng(dsn, owner string, pool ConnPoolConfig) (*OracleDamengReader, error) {
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	pool.applyTo(db)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	r := &OracleReader{db: db, owner: strings.ToUpper(owner), version: 11}
	var version string
	if err = db.QueryRow(`SELECT VERSION FROM V$INSTANCE`).Scan(&version); err == nil {
		if v, e := strconv.Atoi(strings.SplitN(version, ".", 2)[0]); e == nil && v > 0 {
			r.version = v
		}
	}
	return WrapOracleForDameng(r), nil
}

func (r *OracleDamengReader) MigrationSchema() string { return r.owner }

func quoteOracleDameng(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (r *OracleDamengReader) tableName(t string) string {
	return quoteOracleDameng(r.owner) + "." + quoteOracleDameng(t)
}

func (r *OracleDamengReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	r.metadataMu.RLock()
	cached := r.metadata[table]
	r.metadataMu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT c.COLUMN_NAME, c.DATA_TYPE,
 NVL(c.CHAR_LENGTH,0), c.DATA_LENGTH, NVL(c.DATA_PRECISION,0), NVL(c.DATA_SCALE,0),
 c.NULLABLE, c.DATA_DEFAULT, NVL(c.CHAR_USED,'B'), c.COLUMN_ID, c.DATA_TYPE_OWNER, vc.VIRTUAL_COLUMN
 FROM ALL_TAB_COLUMNS c JOIN ALL_TAB_COLS vc
 ON vc.OWNER=c.OWNER AND vc.TABLE_NAME=c.TABLE_NAME AND vc.COLUMN_NAME=c.COLUMN_NAME
 WHERE c.OWNER=:1 AND c.TABLE_NAME=:2 ORDER BY c.COLUMN_ID`, r.owner, table)
	if err != nil {
		return nil, err
	}
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var c ColumnInfo
		var nullable, virtual string
		var def, typeOwner sql.NullString
		if err = rows.Scan(&c.Name, &c.DataType, &c.Length, &c.DataLength, &c.Precision, &c.Scale,
			&nullable, &def, &c.CharUsed, &c.Ordinal, &typeOwner, &virtual); err != nil {
			rows.Close()
			return nil, err
		}
		c.IsNullable = nullable == "Y"
		c.FormattedType = c.DataType
		if c.Length == 0 {
			c.Length = c.DataLength
		}
		if strings.HasPrefix(strings.ToUpper(c.DataType), "TIMESTAMP") {
			c.DateTimePrecision = c.Scale
		}
		if def.Valid && strings.TrimSpace(def.String) != "" {
			d := strings.TrimSpace(def.String)
			c.Default = &d
		}
		if typeOwner.Valid && typeOwner.String != "" {
			c.TypeSchema, c.TypeKind = typeOwner.String, "user_defined"
		}
		if virtual == "YES" {
			c.Generated = "virtual"
		}
		info.Columns = append(info.Columns, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(info.Columns) == 0 {
		return nil, fmt.Errorf("表 %s 不存在或列元数据不可见", table)
	}
	if r.version >= 12 {
		identities, e := r.db.QueryContext(ctx, `SELECT i.COLUMN_NAME, i.GENERATION_TYPE, c.DEFAULT_ON_NULL
 FROM ALL_TAB_IDENTITY_COLS i JOIN ALL_TAB_COLUMNS c
 ON c.OWNER=i.OWNER AND c.TABLE_NAME=i.TABLE_NAME AND c.COLUMN_NAME=i.COLUMN_NAME
 WHERE i.OWNER=:1 AND i.TABLE_NAME=:2`, r.owner, table)
		if e != nil {
			return nil, e
		}
		for identities.Next() {
			var name, generation, onNull string
			if e = identities.Scan(&name, &generation, &onNull); e != nil {
				identities.Close()
				return nil, e
			}
			for i := range info.Columns {
				if info.Columns[i].Name == name {
					info.Columns[i].Identity = oracleDamengGeneration(generation, onNull)
					// The internal sequence default is installed through its binding,
					// after bulk loading explicit source identity values.
					info.Columns[i].Default = nil
				}
			}
		}
		e = identities.Err()
		identities.Close()
		if e != nil {
			return nil, e
		}
	}
	r.metadataMu.Lock()
	if r.metadata == nil {
		r.metadata = map[string]*TableDDLInfo{}
	}
	r.metadata[table] = info
	r.metadataMu.Unlock()
	return info, nil
}

func oracleDamengProjection(c ColumnInfo) string {
	name := quoteOracleDameng(c.Name)
	t := strings.ToUpper(c.DataType)
	expression := name
	switch {
	case t == "NUMBER" || t == "FLOAT" || t == "DECIMAL" || t == "NUMERIC":
		expression = "TO_CHAR(" + name + ", 'TM9', 'NLS_NUMERIC_CHARACTERS=''.,''')"
	case t == "DATE":
		expression = "TRIM(TO_CHAR(" + name + ", 'SYYYY-MM-DD HH24:MI:SS', 'NLS_DATE_LANGUAGE=English'))"
	case strings.HasPrefix(t, "TIMESTAMP"):
		format := "SYYYY-MM-DD HH24:MI:SS.FF9"
		if strings.Contains(t, "LOCAL TIME ZONE") {
			// A fixed offset retains the instant independently of session timezone.
			name += " AT TIME ZONE '+00:00'"
		}
		if strings.Contains(t, "TIME ZONE") {
			format += " TZH:TZM"
		}
		expression = "TRIM(TO_CHAR(" + name + ", '" + format + "', 'NLS_DATE_LANGUAGE=English'))"
	case strings.HasPrefix(t, "INTERVAL"):
		expression = "TO_CHAR(" + name + ")"
	}
	return expression + " AS " + quoteOracleDameng(c.Name)
}

func (r *OracleDamengReader) ReadPage(ctx context.Context, table string, pkCols []string, offset, limit int64) ([]string, []string, [][]interface{}, error) {
	if offset < 0 || limit <= 0 {
		return nil, nil, nil, fmt.Errorf("无效分页范围")
	}
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	cols, types, projections := []string{}, []string{}, []string{}
	for _, c := range info.Columns {
		if c.Generated != "" {
			return nil, nil, nil, fmt.Errorf("表 %s 列 %s 为虚拟列，暂不支持迁移", table, c.Name)
		}
		cols = append(cols, c.Name)
		types = append(types, strings.ToUpper(c.DataType))
		projections = append(projections, oracleDamengProjection(c))
	}
	order := "ROWID"
	if len(pkCols) > 0 {
		names := make([]string, len(pkCols))
		for i, pk := range pkCols {
			names[i] = quoteOracleDameng(pk)
		}
		order = strings.Join(names, ", ")
	}
	base := "SELECT " + strings.Join(projections, ", ") + " FROM " + r.tableName(table) + " ORDER BY " + order
	query := fmt.Sprintf("%s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", base, offset, limit)
	if r.version < 12 {
		// Explicit outer projection avoids a collision with a source RN column.
		names := make([]string, len(cols))
		for i, name := range cols {
			names[i] = "DBG_PAGE." + quoteOracleDameng(name)
		}
		pseudo := "DBGOLD_PAGE_ROWNUM"
		for {
			collision := false
			for _, name := range cols {
				collision = collision || name == pseudo
			}
			if !collision {
				break
			}
			pseudo += "_"
		}
		query = fmt.Sprintf("SELECT %s FROM (SELECT DBG_INNER.*, ROWNUM %s FROM (%s) DBG_INNER WHERE ROWNUM <= %d) DBG_PAGE WHERE DBG_PAGE.%s > %d", strings.Join(names, ", "), quoteOracleDameng(pseudo), base, offset+limit, quoteOracleDameng(pseudo), offset)
	}
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var out [][]interface{}
	for rows.Next() {
		values, pointers := make([]interface{}, len(cols)), make([]interface{}, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return nil, nil, nil, err
		}
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				// database/sql driver buffers can be reused at the next Scan.
				values[i] = append([]byte{}, b...)
			}
		}
		out = append(out, values)
	}
	return cols, types, out, rows.Err()
}

func (r *OracleDamengReader) CountRows(ctx context.Context, table string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+r.tableName(table)).Scan(&count)
	return count, err
}

var _ Reader = (*OracleDamengReader)(nil)
