package dialect

import (
	"crypto/sha256"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"

	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

func (d *DaMengDialect) SequenceName(seq source.SequenceInfo) string {
	if seq.Native != nil && seq.Native.Name != "" {
		return strings.ToUpper(seq.Native.Name)
	}
	return "seq_" + seq.TableName + "_" + seq.ColumnName
}

func OracleDamengTriggerName(seq source.SequenceInfo, b source.SequenceBinding) string {
	if b.TriggerName != "" {
		return strings.ToUpper(b.TriggerName)
	}
	name := "DBGOLD_TRG_" + b.TableName + "_" + b.ColumnName
	if len(name) > 128 {
		sum := sha256.Sum256([]byte(name))
		name = fmt.Sprintf("DBGOLD_TRG_%x", sum[:16])
	}
	return strings.ToUpper(name)
}

// NormalizeOracleDamengSequence intersects noncycling source limits with
// Dameng's signed 64-bit range. Actual starts and increments remain unchanged.
// The returned native metadata and bindings never alias the source metadata.
func NormalizeOracleDamengSequence(seq source.SequenceInfo) (source.SequenceInfo, error) {
	if seq.Native != nil {
		n := *seq.Native
		n.Bindings = slices.Clone(n.Bindings)
		seq.Native = &n
		if !n.Cycle {
			minimum, maximum := big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
			if lo, ok := new(big.Int).SetString(n.MinValue, 10); ok && lo.Cmp(minimum) < 0 {
				n.MinValue = minimum.String()
			}
			if hi, ok := new(big.Int).SetString(n.MaxValue, 10); ok && hi.Cmp(maximum) > 0 {
				n.MaxValue = maximum.String()
			}
		}
	}
	return seq, ValidateOracleDamengSequence(seq)
}

// ValidateOracleDamengSequence must run before rendering or executing native DDL.
func ValidateOracleDamengSequence(seq source.SequenceInfo) error {
	if seq.Unsupported != "" {
		return fmt.Errorf("%s", seq.Unsupported)
	}
	if seq.Native == nil {
		return fmt.Errorf("缺少 Oracle 命名序列元数据")
	}
	n := seq.Native
	if n.Cycle {
		return fmt.Errorf("序列 %s 为循环序列，无法安全推导迁移后的续号", n.Name)
	}
	if err := tsqldameng.ValidName(n.Name); err != nil {
		return err
	}
	parse := func(s, label string) (*big.Int, error) {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok || !v.IsInt64() {
			return nil, fmt.Errorf("序列 %s 的%s超出达梦 int64 范围: %s", n.Name, label, s)
		}
		return v, nil
	}
	start := seq.StartValueText
	if start == "" {
		start = fmt.Sprint(seq.StartValue)
	}
	a, err := parse(start, "起始值")
	if err != nil {
		return err
	}
	step, err := parse(n.IncrementBy, "步长")
	if err != nil {
		return err
	}
	lo, err := parse(n.MinValue, "最小值")
	if err != nil {
		return err
	}
	hi, err := parse(n.MaxValue, "最大值")
	if err != nil {
		return err
	}
	if step.Sign() == 0 || step.String() == "-9223372036854775808" || lo.Cmp(hi) >= 0 || a.Cmp(lo) < 0 || a.Cmp(hi) > 0 || new(big.Int).Abs(step).Cmp(new(big.Int).Sub(hi, lo)) > 0 {
		return fmt.Errorf("序列 %s 起值、步长或边界无效", n.Name)
	}
	cache, ok := new(big.Int).SetString(n.CacheSize, 10)
	if !ok || cache.Sign() < 0 || !cache.IsInt64() || cache.Int64() > 2147483647 || cache.Int64() == 1 {
		return fmt.Errorf("序列 %s 缓存值无效", n.Name)
	}
	seen := map[string]bool{}
	for _, b := range n.Bindings {
		if b.Kind != "trigger" && !(b.Kind == "identity" && b.OnlyWhenNull) {
			return fmt.Errorf("序列 %s 绑定 %s.%s 的生成模式不受支持", n.Name, b.TableName, b.ColumnName)
		}
		for _, name := range []string{b.TableName, b.ColumnName, OracleDamengTriggerName(seq, b)} {
			if err := tsqldameng.ValidName(name); err != nil {
				return err
			}
		}
		key := OracleDamengTriggerName(seq, b)
		if seen[key] {
			return fmt.Errorf("序列 %s 包含重复触发器 %s", n.Name, key)
		}
		seen[key] = true
	}
	return nil
}

// SequenceStatements reattaches IDENTITY using DM8's documented ADD COLUMN
// identity clause. ALTER COLUMN ... RESTART WITH is not DM8 syntax.
func (d *DaMengDialect) SequenceStatements(schema string, seq source.SequenceInfo, _ string) []Statement {
	if seq.Native != nil {
		var err error
		seq, err = NormalizeOracleDamengSequence(seq)
		if err != nil {
			return nil
		}
		n := seq.Native
		start := seq.StartValueText
		if start == "" {
			start = fmt.Sprint(seq.StartValue)
		}
		cycle, order, cache := "NOCYCLE", "NOORDER", "NOCACHE"
		if n.Cycle {
			cycle = "CYCLE"
		}
		if n.Ordered {
			order = "ORDER"
		}
		if n.CacheSize != "0" {
			cache = "CACHE " + n.CacheSize
		}
		qualified := d.QualifyTable(schema, n.Name)
		statements := []Statement{{SQL: fmt.Sprintf("CREATE SEQUENCE %s START WITH %s INCREMENT BY %s MINVALUE %s MAXVALUE %s %s %s %s", qualified, start, n.IncrementBy, n.MinValue, n.MaxValue, cycle, cache, order)}}
		for _, b := range n.Bindings {
			assignment := "  SELECT " + qualified + ".NEXTVAL INTO :NEW." + d.QuoteIdent(b.ColumnName) + " FROM DUAL;\n"
			if b.OnlyWhenNull {
				assignment = "  IF :NEW." + d.QuoteIdent(b.ColumnName) + " IS NULL THEN\n  " + assignment + "  END IF;\n"
			}
			marker := strings.ReplaceAll("DBGOLD_ORACLE_DAMENG:"+strings.ToUpper(n.Name), "*/", "* /")
			statements = append(statements, Statement{SQL: "CREATE TRIGGER " + d.QualifyTable(schema, OracleDamengTriggerName(seq, b)) + " BEFORE INSERT ON " + d.QualifyTable(schema, b.TableName) + " FOR EACH ROW\nBEGIN\n  /* " + marker + " */\n" + assignment + "END;"})
		}
		return statements
	}
	step := int64(1)
	if seq.IdentityInfo != nil {
		step = seq.IdentityInfo.Increment
	}
	table := d.QualifyTable(schema, seq.TableName)
	return []Statement{
		{SQL: "ALTER TABLE " + table + " DROP IDENTITY"},
		{SQL: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s IDENTITY(%d, %d)", table, d.QuoteIdent(seq.ColumnName), seq.StartValue, step)},
	}
}

// IndexStatements 达梦索引/主键 DDL,不带 IF NOT EXISTS。
// 注意:达梦索引名在 schema 内全局唯一(不同于 MySQL 的表级作用域),
// 故非主键索引名加「表名_」前缀去重,避免多表同名索引(如 row_id、ix_tmp_autoinc)冲突。
func (d *DaMengDialect) IndexStatements(schema string, idx source.IndexInfo) []Statement {
	quotedCols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		quotedCols[i] = d.QuoteIdent(c)
		if !idx.IsPrimary && i < len(idx.Descending) && idx.Descending[i] {
			quotedCols[i] += " DESC"
		}
	}
	cols := strings.Join(quotedCols, ", ")
	qn := d.QualifyTable(schema, idx.TableName)
	var sql string
	switch {
	case idx.IsPrimary:
		sql = fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s)", qn, cols)
	case idx.IsUnique:
		sql = fmt.Sprintf(`CREATE UNIQUE INDEX %s ON %s (%s)`, d.QuoteIdent(d.uniqueIndexName(idx)), qn, cols)
	default:
		sql = fmt.Sprintf(`CREATE INDEX %s ON %s (%s)`, d.QuoteIdent(d.uniqueIndexName(idx)), qn, cols)
	}
	return []Statement{{SQL: sql}}
}

// uniqueIndexName 为达梦生成 schema 级唯一的索引名:「表名_索引名」。
func (d *DaMengDialect) uniqueIndexName(idx source.IndexInfo) string {
	return idx.TableName + "_" + idx.IndexName
}

// IndexName keeps report and repair object names aligned with the emitted DDL.
func (d *DaMengDialect) IndexName(idx source.IndexInfo) string {
	if idx.IsPrimary {
		return idx.IndexName
	}
	return strings.ToUpper(d.uniqueIndexName(idx))
}

// ForeignKeyStatements 达梦外键 DDL。达梦/Oracle 不支持 ON UPDATE 子句,予以丢弃;
// ON DELETE 仅在非 NO ACTION 时输出(NO ACTION 为默认,可省略)。
func (d *DaMengDialect) ForeignKeyStatements(schema string, fk source.FKInfo) []Statement {
	quotedCols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quotedCols[i] = d.QuoteIdent(c)
	}
	quotedRefCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		quotedRefCols[i] = d.QuoteIdent(c)
	}
	sql := fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)`,
		d.QualifyTable(schema, fk.TableName), d.QuoteIdent(fk.ConstraintName),
		strings.Join(quotedCols, ", "),
		d.QualifyTable(schema, fk.RefTable),
		strings.Join(quotedRefCols, ", "))
	if od := strings.ToUpper(strings.TrimSpace(fk.OnDelete)); od != "" && od != "NO ACTION" {
		sql += " ON DELETE " + od
	}
	return []Statement{{SQL: sql}}
}

// ViewStatements 达梦视图 DDL。注意:MySQL 视图定义为 MySQL 方言,
// 在达梦下可能因函数/语法差异执行失败,失败由上层记入 report(已知限制)。
func (d *DaMengDialect) ViewStatements(schema string, view source.ViewInfo) []Statement {
	columns := ""
	if len(view.Columns) > 0 {
		names := make([]string, len(view.Columns))
		for i, c := range view.Columns {
			names[i] = d.QuoteIdent(c.Name)
		}
		columns = " (" + strings.Join(names, ", ") + ")"
	}
	sql := fmt.Sprintf("CREATE OR REPLACE VIEW %s%s AS %s", d.QualifyTable(schema, view.ViewName), columns, view.Definition)
	return []Statement{{SQL: sql}}
}

// CommentStatements 达梦支持 COMMENT ON TABLE/COLUMN,语法同 PostgreSQL,均不需要列类型。
func (d *DaMengDialect) CommentStatements(schema string, cm source.CommentInfo) []Statement {
	val := strings.ReplaceAll(cm.Comment, "'", "''")
	var sql string
	if cm.ColumnName == "" {
		sql = fmt.Sprintf("COMMENT ON TABLE %s IS '%s'", d.QualifyTable(schema, cm.TableName), val)
	} else {
		sql = fmt.Sprintf("COMMENT ON COLUMN %s.%s IS '%s'",
			d.QualifyTable(schema, cm.TableName), d.QuoteIdent(cm.ColumnName), val)
	}
	return []Statement{{SQL: sql}}
}

// AdjustViewDefinition 把中间形式 gen_random_uuid() 替换为达梦的 sys_guid()。
func (d *DaMengDialect) AdjustViewDefinition(def string) string {
	return reGenRandomUUID.ReplaceAllString(def, "sys_guid()")
}
