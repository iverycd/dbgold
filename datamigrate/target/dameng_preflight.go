package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
)

// ValidateDamengColumns checks the complete target column set before reusing a
// deterministic rename plan in data/object/view-only work. Never issues DDL.
func (w *DaMengWriter) ValidateDamengColumns(ctx context.Context, info *source.TableDDLInfo) error {
	rows, err := w.db.QueryContext(ctx, `SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? ORDER BY COLUMN_ID`, w.schema, strings.ToUpper(info.TableName))
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		actual[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(info.Columns) {
		return fmt.Errorf("目标表 %s 不存在或列数与自动修复映射不一致", info.TableName)
	}
	for _, c := range info.Columns {
		if !actual[c.Name] {
			return fmt.Errorf("目标表 %s 缺少映射列 %s；未修改已有表，请核对列名修复记录", info.TableName, c.Name)
		}
	}
	return nil
}

// ValidateSQLServerDamengExisting rejects implicit narrowing on data/object-only
// runs. Column metadata is observational; no target DDL is executed here.
func (w *DaMengWriter) ValidateSQLServerDamengExisting(ctx context.Context, info *source.TableDDLInfo, opt dialect.TypeOpt) error {
	rows, e := w.db.QueryContext(ctx, `SELECT c.COLUMN_NAME,c.DATA_TYPE,NVL(c.CHAR_LENGTH,0),NVL(c.DATA_LENGTH,0),
 NVL(c.DATA_PRECISION,0),NVL(c.DATA_SCALE,0),c.NULLABLE,NVL(c.CHAR_USED,'B'),
 CASE WHEN sc.INFO2 & 0x01=0x01 THEN 1 ELSE 0 END
 FROM ALL_TAB_COLUMNS c JOIN ALL_OBJECTS o ON o.object_name=c.TABLE_NAME AND o.owner=c.OWNER
 JOIN SYS.SYSCOLUMNS sc ON sc.id=o.object_id AND sc.name=c.COLUMN_NAME
 WHERE c.OWNER=? AND c.TABLE_NAME=? ORDER BY c.COLUMN_ID`, w.schema, strings.ToUpper(info.TableName))
	if e != nil {
		return e
	}
	defer rows.Close()
	type column struct {
		name, typ                      string
		chars, bytes, precision, scale int64
		nullable, units                string
		identity                       int
	}
	actual := map[string]column{}
	for rows.Next() {
		var c column
		if e = rows.Scan(&c.name, &c.typ, &c.chars, &c.bytes, &c.precision, &c.scale, &c.nullable, &c.units, &c.identity); e != nil {
			return e
		}
		actual[c.name] = c
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if len(actual) != len(info.Columns) {
		return fmt.Errorf("目标表 %s 不存在或列数不一致", info.TableName)
	}
	for _, src := range info.Columns {
		c, ok := actual[strings.ToUpper(src.Name)]
		if !ok {
			return fmt.Errorf("目标缺少列 %s.%s", info.TableName, src.Name)
		}
		want := typemap.SQLServerToDameng(src, opt.CharInLength, opt.UseNvarchar2)
		if dmBaseType(c.typ) != dmBaseType(want) {
			return fmt.Errorf("目标列 %s.%s 类型 %s 与所需 %s 不匹配", info.TableName, src.Name, c.typ, want)
		}
		if src.IsNullable && c.nullable != "Y" {
			return fmt.Errorf("目标列 %s.%s 不允许 NULL", info.TableName, src.Name)
		}
		if (src.Extra == "auto_increment") != (c.identity == 1) {
			return fmt.Errorf("目标列 %s.%s 自增属性不一致", info.TableName, src.Name)
		}
		switch dmBaseType(want) {
		case "CHAR", "VARCHAR", "NVARCHAR2":
			required := src.Length
			if strings.EqualFold(src.DataType, "uniqueidentifier") {
				required = 36
			}
			capacity := c.bytes
			if c.units == "C" || dmBaseType(want) == "NVARCHAR2" {
				capacity = c.chars
			}
			if capacity < required || opt.CharInLength && !strings.EqualFold(src.DataType, "uniqueidentifier") && c.units != "C" && dmBaseType(want) != "NVARCHAR2" {
				return fmt.Errorf("目标列 %s.%s 长度或字符单位不足", info.TableName, src.Name)
			}
		case "NUMBER":
			p, s := src.Precision, src.Scale
			switch strings.ToLower(src.DataType) {
			case "bit":
				p, s = 1, 0
			case "money":
				p, s = 19, 4
			case "smallmoney":
				p, s = 10, 4
			}
			if c.precision <= 0 || c.scale < s || c.precision-c.scale < p-s {
				return fmt.Errorf("目标列 %s.%s 数值精度不足", info.TableName, src.Name)
			}
		case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIME":
			precision := src.DateTimePrecision
			if strings.EqualFold(src.DataType, "datetime") {
				precision = 3
			}
			if c.scale < precision {
				return fmt.Errorf("目标列 %s.%s 小数秒精度不足", info.TableName, src.Name)
			}
		case "VARBINARY":
			if c.bytes < 8 {
				return fmt.Errorf("目标列 %s.%s 无法容纳 rowversion", info.TableName, src.Name)
			}
		}
	}
	return nil
}

var dmTypeSize = regexp.MustCompile(`\([^)]*\)`)

func dmBaseType(s string) string {
	s = strings.Join(strings.Fields(dmTypeSize.ReplaceAllString(strings.ToUpper(s), "")), " ")
	switch s {
	case "VARCHAR2":
		return "VARCHAR"
	case "INTEGER":
		return "INT"
	case "DECIMAL", "NUMERIC", "DEC":
		return "NUMBER"
	case "DATETIME":
		return "TIMESTAMP"
	case "DOUBLE PRECISION":
		return "DOUBLE"
	}
	return s
}

func (w *DaMengWriter) SQLServerDamengIdentityBounds(ctx context.Context, table, column string) (*int64, *int64, error) {
	var lo, hi sql.NullInt64
	col := w.dia.QuoteIdent(column)
	err := w.db.QueryRowContext(ctx, "SELECT MIN("+col+"), MAX("+col+") FROM "+w.qualifiedTable(table)).Scan(&lo, &hi)
	if err != nil {
		return nil, nil, err
	}
	if !lo.Valid {
		return nil, nil, nil
	}
	return &lo.Int64, &hi.Int64, nil
}
