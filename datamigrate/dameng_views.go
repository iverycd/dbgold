package datamigrate

import (
	"fmt"
	"slices"
	"strings"

	"dbgold/datamigrate/oracledameng"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/tsqldameng"
)

func (m *Migrator) damengSourceSchema() string {
	if m.isSQLServerDameng() {
		return "dbo"
	}
	return m.dmSourceSchema
}

// Prepare the repair editor's target definition once, before either caller
// wraps it in CREATE VIEW. Table-column repair plans do not change view SQL.
func (m *Migrator) prepareDamengView(v source.ViewInfo) (source.ViewInfo, error) {
	v.ViewName = m.objName(v.ViewName)
	if m.isOscarDameng() {
		reason := "神通到达梦的视图需要手工适配"
		if v.Unsupported != "" {
			reason += "；" + v.Unsupported
		}
		return v, fmt.Errorf("%s", reason)
	}
	if v.Unsupported != "" {
		return v, fmt.Errorf("%s", v.Unsupported)
	}
	if strings.TrimSpace(v.Definition) == "" {
		return v, fmt.Errorf("源视图定义为空或不可读取")
	}
	if m.isSQLServerDameng() {
		sql, _, err := tsqldameng.View(v.Definition, m.cfg.TargetSchema)
		if err != nil {
			return v, err
		}
		v.Definition = sql
		return v, nil
	}
	// Oracle's reader supplies a SELECT body plus its declared output names,
	// not a source CREATE statement. Apply the normal Oracle dialect adaptation
	// when available. The repair candidate for other expressions uses this same
	// query body in a target-schema CREATE VIEW.
	converted, err := oracledameng.ConvertViewWithOptions(v.Definition, m.damengSourceSchema(), m.cfg.TargetSchema, nil, m.oracleDamengSQLOptions())
	if err == nil {
		v.Definition = converted.SQL
	}
	v.Columns = slices.Clone(v.Columns)
	for i := range v.Columns {
		v.Columns[i].Name = m.objName(v.Columns[i].Name)
	}
	return v, nil
}
