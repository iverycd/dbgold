package source

import (
	"context"
	"database/sql"
	"dbgold/datamigrate/validation"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// CrossProfile describes observed capabilities, never a certification claim.
type CrossProfile struct{ Product, Version, Mode, Schema, Charset, LengthUnit, CaseSensitive string }

type SQLServerOracleReader struct{ *SQLServerDamengReader }

func NewSQLServerForOracle(dsn, database string, pool ConnPoolConfig) (*SQLServerOracleReader, error) {
	r, err := NewSQLServerForDameng(dsn, database, pool)
	if err != nil {
		return nil, err
	}
	return &SQLServerOracleReader{r}, nil
}
func (r *SQLServerOracleReader) SourceSchema() string    { return "dbo" }
func (r *SQLServerOracleReader) MigrationSchema() string { return "dbo" }
func (r *SQLServerOracleReader) CrossProfile(ctx context.Context) (CrossProfile, error) {
	p := CrossProfile{Product: "sqlserver", Schema: "dbo"}
	err := r.db.QueryRowContext(ctx, `SELECT CONVERT(varchar(128),SERVERPROPERTY('ProductVersion'))`).Scan(&p.Version)
	if err == nil {
		var major int
		fmt.Sscanf(p.Version, "%d", &major)
		if major < 11 {
			err = fmt.Errorf("SQL Server 源版本必须为 2012 或以上")
		}
	}
	return p, err
}
func (r *SQLServerOracleReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.name,ty.name,CASE WHEN ty.name IN ('nchar','nvarchar') AND c.max_length>0 THEN c.max_length/2 ELSE c.max_length END,
 c.precision,c.scale,c.is_nullable,dc.definition,c.is_computed,ty.is_user_defined,
 CONVERT(varchar(100),ic.seed_value),CONVERT(varchar(100),ic.increment_value)
 FROM sys.tables t JOIN sys.columns c ON c.object_id=t.object_id JOIN sys.types ty ON ty.user_type_id=c.user_type_id
 LEFT JOIN sys.default_constraints dc ON dc.object_id=c.default_object_id
 LEFT JOIN sys.identity_columns ic ON ic.object_id=c.object_id AND ic.column_id=c.column_id
 WHERE t.schema_id=SCHEMA_ID('dbo') AND t.is_ms_shipped=0 AND t.name=@p1 ORDER BY c.column_id`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	info := &TableDDLInfo{TableName: table}
	for rows.Next() {
		var c ColumnInfo
		var def, seed, step sql.NullString
		var computed, custom bool
		if err = rows.Scan(&c.Name, &c.DataType, &c.Length, &c.Precision, &c.Scale, &c.IsNullable, &def, &computed, &custom, &seed, &step); err != nil {
			return nil, err
		}
		if def.Valid {
			c.Default = &def.String
		}
		if computed {
			c.Generated = "computed"
		}
		if custom {
			c.TypeKind = "user_defined"
		}
		c.DateTimePrecision = c.Scale
		if seed.Valid {
			c.Extra = "auto_increment"
			c.SQLServerIdentitySeed = seed.String
			c.SQLServerIdentityIncrement = step.String
		}
		info.Columns = append(info.Columns, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(info.Columns) == 0 {
		return nil, fmt.Errorf("dbo.%s 不存在或无列权限", table)
	}
	return info, nil
}
func (r *SQLServerOracleReader) ReadPage(ctx context.Context, table string, pk []string, offset, limit int64) ([]string, []string, [][]any, error) {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	return crossReadPage(ctx, r.db, info, pk, offset, limit, "sqlserver", "dbo")
}

type DaMengCrossReader struct{ *DaMengReader }

func NewDaMengCross(dsn, schema string, pool ConnPoolConfig) (*DaMengCrossReader, error) {
	r, err := NewDaMeng(dsn, schema, pool)
	if err != nil {
		return nil, err
	}
	return &DaMengCrossReader{r}, nil
}
func (r *DaMengCrossReader) SourceSchema() string    { return r.schema }
func (r *DaMengCrossReader) MigrationSchema() string { return r.schema }
func (r *DaMengCrossReader) CrossProfile(ctx context.Context) (CrossProfile, error) {
	p := CrossProfile{Product: "dameng", Schema: r.schema}
	if err := r.db.QueryRowContext(ctx, `SELECT BANNER FROM V$VERSION WHERE ROWNUM=1`).Scan(&p.Version); err != nil {
		return p, err
	}
	version := regexp.MustCompile(`(?i)\bV(\d+)`).FindStringSubmatch(p.Version)
	if len(version) != 2 {
		return p, fmt.Errorf("无法确认 DM8+ 源版本: %s", p.Version)
	}
	major, _ := strconv.Atoi(version[1])
	if major < 8 {
		return p, fmt.Errorf("要求 DM8 或以上版本")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT PARA_NAME,PARA_VALUE FROM V$DM_INI WHERE PARA_NAME IN ('COMPATIBLE_MODE','LENGTH_IN_CHAR','UNICODE_FLAG','CASE_SENSITIVE')`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err = rows.Scan(&name, &value); err != nil {
			return p, err
		}
		switch name {
		case "COMPATIBLE_MODE":
			p.Mode = value
		case "LENGTH_IN_CHAR":
			p.LengthUnit = value
		case "UNICODE_FLAG":
			p.Charset = value
		case "CASE_SENSITIVE":
			p.CaseSensitive = value
		}
	}
	if p.Mode == "" {
		return p, fmt.Errorf("无法探测达梦兼容模式")
	}
	return p, rows.Err()
}
func (r *DaMengCrossReader) GetTableDDLInfo(ctx context.Context, table string) (*TableDDLInfo, error) {
	info, err := r.DaMengReader.GetTableDDLInfo(ctx, table)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT COLUMN_NAME,DATA_LENGTH,CHAR_USED,DATA_SCALE,VIRTUAL_COLUMN FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var length, scale sql.NullInt64
		var unit, generated sql.NullString
		if err = rows.Scan(&name, &length, &unit, &scale, &generated); err != nil {
			return nil, err
		}
		for i := range info.Columns {
			c := &info.Columns[i]
			if c.Name == name {
				c.DataLength = length.Int64
				c.CharUsed = unit.String
				c.DateTimePrecision = scale.Int64
				if generated.String == "YES" {
					c.Generated = "virtual"
				}
			}
		}
	}
	return info, rows.Err()
}
func (r *DaMengCrossReader) GetViews(ctx context.Context) ([]ViewInfo, error) {
	return r.GetViewsForTarget(ctx, "")
}
func (r *DaMengCrossReader) GetViewsForTarget(ctx context.Context, _ string) ([]ViewInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT VIEW_NAME,TEXT FROM ALL_VIEWS WHERE OWNER=? ORDER BY VIEW_NAME`, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ViewInfo
	for rows.Next() {
		var v ViewInfo
		var def sql.NullString
		if err = rows.Scan(&v.ViewName, &def); err != nil {
			return nil, err
		}
		v.Definition = def.String
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return r.crossViewColumns(ctx, out)
}
func (r *DaMengCrossReader) ReadPage(ctx context.Context, table string, pk []string, offset, limit int64) ([]string, []string, [][]any, error) {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return nil, nil, nil, err
	}
	return crossReadPage(ctx, r.db, info, pk, offset, limit, "dameng", r.schema)
}
func crossQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func crossReadPage(ctx context.Context, db *sql.DB, info *TableDDLInfo, pk []string, offset, limit int64, product, schema string) ([]string, []string, [][]any, error) {
	quote := crossQuote
	if product == "sqlserver" {
		quote = quoteMSSQL
	}
	cols, types, projection := crossProjection(info, product)
	order := []string{}
	for _, c := range pk {
		order = append(order, quote(c))
	}
	if len(order) == 0 {
		if product == "sqlserver" {
			order = []string{"(SELECT NULL)"}
		} else {
			order = []string{"ROWID"}
		}
	}
	q := "SELECT " + strings.Join(projection, ",") + " FROM " + quote(schema) + "." + quote(info.TableName) + " ORDER BY " + strings.Join(order, ",")
	var rows *sql.Rows
	var err error
	if product == "sqlserver" {
		rows, err = db.QueryContext(ctx, q+" OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY", offset, limit)
	} else {
		rows, err = db.QueryContext(ctx, q+" OFFSET ? ROWS FETCH NEXT ? ROWS ONLY", offset, limit)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var data [][]any
	for rows.Next() {
		v := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range v {
			ptr[i] = &v[i]
		}
		if err = rows.Scan(ptr...); err != nil {
			return nil, nil, nil, err
		}
		for i, x := range v {
			if b, ok := x.([]byte); ok {
				v[i] = append([]byte{}, b...)
			}
		}
		data = append(data, v)
	}
	return cols, types, data, rows.Err()
}

func crossNext(seed, step string, last sql.NullString) (string, error) {
	n, ok := new(big.Int).SetString(seed, 10)
	if !ok {
		return "", fmt.Errorf("无效自增种子 %q", seed)
	}
	d, ok := new(big.Int).SetString(step, 10)
	if !ok || d.Sign() == 0 {
		return "", fmt.Errorf("无效自增步长 %q", step)
	}
	if last.Valid {
		n, ok = new(big.Int).SetString(last.String, 10)
		if !ok {
			return "", fmt.Errorf("无效自增状态")
		}
		n.Add(n, d)
	}
	return n.String(), nil
}

func crossProjection(info *TableDDLInfo, product string) ([]string, []string, []string) {
	quote := crossQuote
	if product == "sqlserver" {
		quote = quoteMSSQL
	}
	cols, types, projection := []string{}, []string{}, []string{}
	for _, c := range info.Columns {
		q := quote(c.Name)
		t := strings.ToLower(c.DataType)
		exact := t == "decimal" || t == "numeric" || t == "number" || t == "money" || t == "smallmoney" || c.DamengFloatStorage == "decimal"
		if exact {
			if product == "sqlserver" {
				q = "CONVERT(varchar(256)," + q + ")"
			} else {
				q = "CAST(" + q + " AS VARCHAR(256))"
			}
		}
		if product == "sqlserver" && t == "uniqueidentifier" {
			q = "CONVERT(varchar(36)," + q + ")"
		}
		projection = append(projection, q+" AS "+quote(c.Name))
		cols = append(cols, c.Name)
		types = append(types, strings.ToUpper(c.DataType))
	}
	return cols, types, projection
}
func (r *SQLServerOracleReader) StreamCrossRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return err
	}
	_, types, projection := crossProjection(info, "sqlserver")
	return streamPartitionQuery(ctx, r.db, "SELECT "+strings.Join(projection, ",")+" FROM "+dboTable(table), batch, types, nil, consume)
}
func (r *DaMengCrossReader) StreamCrossRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	info, err := r.GetTableDDLInfo(ctx, table)
	if err != nil {
		return err
	}
	_, types, projection := crossProjection(info, "dameng")
	return streamPartitionQuery(ctx, r.db, "SELECT "+strings.Join(projection, ",")+" FROM "+crossQuote(r.schema)+"."+crossQuote(table), batch, types, nil, consume)
}
func (r *SQLServerOracleReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	return r.StreamCrossRows(ctx, table, batch, consume)
}
func (r *DaMengCrossReader) StreamPartitionRows(ctx context.Context, table string, batch int, consume func([]string, []string, [][]any) error) error {
	return r.StreamCrossRows(ctx, table, batch, consume)
}
func (r *SQLServerOracleReader) DescribeValidationTable(ctx context.Context, table string) (validation.Table, error) {
	return validation.NewSQLScanner(r.db, "sqlserver", "dbo").DescribeValidationTable(ctx, table)
}
func (r *SQLServerOracleReader) OpenValidationScan(ctx context.Context, table string, cols []validation.Column) (validation.RowStream, error) {
	return validation.NewSQLScanner(r.db, "sqlserver", "dbo").OpenValidationScan(ctx, table, cols)
}
func (r *SQLServerOracleReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	return sqlServerPartitionIndexes(ctx, r.db, table, true)
}
