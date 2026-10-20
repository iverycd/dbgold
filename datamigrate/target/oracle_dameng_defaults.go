package target

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/oracledameng"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

var oracleDMNumericDefault = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// ValidateOracleDamengDefaults checks known constants against the observed
// FLOAT family, before DROP/CREATE. Data-only callers intentionally skip this.
func (w *DaMengWriter) ValidateOracleDamengDefaults(ctx context.Context, infos []*source.TableDDLInfo, opt dialect.TypeOpt) error {
	for _, info := range infos {
		for _, col := range info.Columns {
			base := typemap.OracleDamengBaseType(col.DataType)
			if col.Default == nil || !oracleDMFloatingSource(base) {
				continue
			}
			fail := func(err error) error { return fmt.Errorf("默认值 %s.%s: %w", info.TableName, col.Name, err) }
			clause, err := oracledameng.DefaultWithOptions(*col.Default, oracledameng.Options{Float: true, UseDate: w.OracleDamengUsesDate(), CharInLength: opt.CharInLength, UseNvarchar2: opt.UseNvarchar2})
			if err != nil {
				return fail(err)
			}
			raw := strings.TrimSpace(strings.TrimPrefix(clause, " DEFAULT "))
			if raw == "NULL" {
				continue
			}
			if !oracleDMNumericDefault.MatchString(raw) {
				return fail(fmt.Errorf("浮点默认值仅支持数值常量或 NULL，其他表达式须人工适配"))
			}
			kind, err := w.OracleDamengColumnFloatKind(ctx, info.TableName, col.Name)
			if err != nil {
				return fail(err)
			}
			bits := 64
			if kind == "real" {
				bits = 32
			}
			if _, err := valueconv.OracleDamengFloat(raw, base, kind == "decimal", bits); err != nil {
				return fail(err)
			}
		}
	}
	return nil
}
