package source

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/internal/jdbcbridge"
)

func (r *OscarDamengReader) damengIdentity(ctx context.Context, table string, col jdbcbridge.MigrationColumn) (*IdentityInfo, error) {
	typ := oscarDamengType(col)
	identity := strings.EqualFold(col.AutoIncrement, "YES") || typ == "SERIAL" || typ == "BIGSERIAL"
	var owner, name string
	if col.Default != nil {
		owner, name = oscarSequenceDefault(*col.Default)
	}
	if !identity && name == "" {
		return nil, nil
	}
	errForColumn := func(reason string) error {
		return fmt.Errorf("神通表 %s 自增列 %s: %s", table, col.Name, reason)
	}
	if !identity {
		return nil, errForColumn("源默认表达式引用了未确认归属的序列")
	}
	if name == "" || owner != "" && owner != r.schema {
		return nil, errForColumn("缺少迁移 Schema 内可只读核验的关联序列")
	}
	if col.Nullable {
		return nil, errForColumn("允许 NULL 的自增列无法等价转换为达梦 IDENTITY")
	}
	maximum := int64(math.MaxInt64)
	switch typ {
	case "INT", "SERIAL":
		maximum = math.MaxInt32
	case "BIGINT", "BIGSERIAL":
	default:
		return nil, errForColumn("达梦 IDENTITY 仅支持 INT 和 BIGINT 列")
	}
	if owner == "" {
		owner = r.schema
	}
	state, err := r.readOscarSequenceState(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("读取神通表 %s 自增列 %s 的序列: %w", table, col.Name, err)
	}
	rows, err := r.query(ctx, "SELECT MAX("+oscarQuoteIdent(col.Name)+") FROM "+r.table(table))
	if err != nil {
		return nil, fmt.Errorf("读取神通表 %s 自增列 %s 的最大值: %w", table, col.Name, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return nil, errForColumn("未返回唯一最大值")
	}
	var existingMax int64
	if rows[0][0] != nil {
		existingMax, err = oscarInteger(rows[0][0])
		if err != nil {
			return nil, errForColumn("最大值不是有效整数")
		}
	}
	seed, err := oscarDamengIdentitySeed(state, existingMax, maximum)
	if err != nil {
		return nil, errForColumn(err.Error())
	}
	last := state.last
	res := &IdentityInfo{Seed: seed, Increment: state.step}
	if state.called {
		res.Last = &last
	}
	return res, nil
}

func oscarDamengIdentitySeed(state oscarSequenceState, existingMax, maximum int64) (int64, error) {
	if state.start <= 0 || state.step <= 0 || state.step > maximum || state.cycle ||
		state.maximum < maximum || state.last < state.start || state.last > maximum {
		return 0, fmt.Errorf("源序列的起点、步长、上界或循环属性无法转换为达梦 IDENTITY")
	}
	seed := state.last
	if state.called {
		if state.step > maximum-seed {
			return 0, fmt.Errorf("源自增列的下一个值超出目标整数范围")
		}
		seed += state.step
	}
	if existingMax >= seed {
		steps := (existingMax-seed)/state.step + 1
		if steps > (maximum-seed)/state.step {
			return 0, fmt.Errorf("源表最大值之后没有可用的自增值")
		}
		seed += steps * state.step
	}
	return seed, nil
}

func (r *OscarDamengReader) GetSequences(ctx context.Context) ([]SequenceInfo, error) {
	tables, err := r.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	var out []SequenceInfo
	for _, table := range tables {
		cols, err := r.columns(ctx, table)
		if failure.IsTaskError(err) {
			return nil, err
		}
		if err != nil {
			continue
		}
		for _, col := range cols {
			identity, err := r.damengIdentity(ctx, table, col)
			if failure.IsTaskError(err) {
				return nil, err
			}
			s := SequenceInfo{TableName: table, ColumnName: col.Name}
			if err != nil {
				s.Unsupported = err.Error()
			} else if identity == nil {
				continue
			} else {
				s.StartValue, s.StartValueText, s.IdentityInfo = identity.Seed, strconv.FormatInt(identity.Seed, 10), identity
			}
			out = append(out, s)
		}
	}
	return out, nil
}
