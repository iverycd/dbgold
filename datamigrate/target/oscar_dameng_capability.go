package target

import (
	"context"
	"fmt"
	"strings"

	"dbgold/datamigrate/failure"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/sqlsyntax"
	"dbgold/datamigrate/typemap"
	"dbgold/datamigrate/valueconv"
)

// PrepareOscarDameng checks conversion paths known to vary between DM server
// modes. It reads capabilities before any CREATE or DROP statement is issued.
func (w *DaMengWriter) PrepareOscarDameng(ctx context.Context, info *source.TableDDLInfo) error {
	for _, c := range info.Columns {
		mapped, err := typemap.OscarToDamengChecked(c)
		if err == nil {
			err = w.probeOscarDamengColumn(ctx, c, mapped)
		}
		if err != nil {
			return fmt.Errorf("目标列 %s.%s: %w", info.TableName, c.Name, err)
		}
	}
	return nil
}

func (w *DaMengWriter) probeOscarDamengColumn(ctx context.Context, c source.ColumnInfo, mapped string) error {
	switch dmBaseType(mapped) {
	case "CHAR", "VARCHAR":
		if c.Default != nil {
			tokens, err := sqlsyntax.Lex(*c.Default)
			if err == nil && len(tokens) > 0 && tokens[0].Kind == 's' && tokens[0].Value == "" {
				return w.probeOscarDamengEmptyString(ctx)
			}
		}
	case "FLOAT":
		return w.oscarDamengProbe(ctx, mapped, func() error {
			p, want := int64(53), "double"
			if mapped == "FLOAT(24)" {
				p, want = 24, "real"
			}
			storage, err := w.probeOracleDMFloatType(ctx, mapped, p)
			if err == nil && storage != want {
				err = fmt.Errorf("%s 实际使用 %s 存储，无法保持源浮点语义", mapped, storage)
			}
			return err
		})
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
		p := typemap.OscarDamengTimePrecision(c)
		zoned := strings.Contains(mapped, "WITH TIME ZONE")
		if p > 6 || zoned {
			return w.probeOscarDamengTimestamp(ctx, p, zoned)
		}
	case "TIME WITH TIME ZONE":
		return w.probeOscarDamengTimeZone(ctx)
	case "XMLTYPE":
		return w.oscarDamengProbe(ctx, "xmltype", func() error { return w.probeOracleDMXML(ctx) })
	}
	return nil
}

func (w *DaMengWriter) oscarDamengProbe(ctx context.Context, key string, check func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.features == nil {
		w.oracleDM.features = make(map[string]error)
	}
	key = "oscar:" + key
	if err, ok := w.oracleDM.features[key]; ok {
		return err
	}
	err := check()
	if !failure.IsTaskError(err) {
		w.oracleDM.features[key] = err
	}
	return err
}

func (w *DaMengWriter) probeOscarDamengEmptyString(ctx context.Context) error {
	return w.oscarDamengProbe(ctx, "empty string", func() error {
		var nullified int
		query := `SELECT CASE WHEN '' IS NULL OR CAST(? AS VARCHAR(16 CHAR)) IS NULL THEN 1 ELSE 0 END FROM DUAL`
		if err := w.db.QueryRowContext(ctx, query, "").Scan(&nullified); err != nil {
			return fmt.Errorf("无法核验达梦空字符串绑定: %w", err)
		}
		if nullified != 0 {
			return fmt.Errorf("目标达梦会把空字符串转换为 NULL，无法保留源值")
		}
		return nil
	})
}

func (w *DaMengWriter) probeOscarDamengTimestamp(ctx context.Context, p int64, zoned bool) error {
	return w.oscarDamengProbe(ctx, fmt.Sprintf("timestamp:%d:%t", p, zoned), func() error {
		return w.probeOracleDMTimestamp(ctx, p, zoned)
	})
}

func (w *DaMengWriter) probeOscarDamengTimeZone(ctx context.Context) error {
	return w.oscarDamengProbe(ctx, "time with time zone", func() error {
		const value = "03:04:05.123456 +05:45"
		query := `SELECT TO_CHAR(` + valueconv.OscarDamengPlaceholder("TIME WITH TIME ZONE") + `, 'HH24:MI:SS.FF6 TZH:TZM') FROM DUAL`
		var actual string
		if err := w.db.QueryRowContext(ctx, query, value).Scan(&actual); err != nil {
			return fmt.Errorf("无法核验达梦 TIME WITH TIME ZONE 绑定: %w", err)
		}
		if actual != value {
			return fmt.Errorf("达梦 TIME WITH TIME ZONE 会改变小数秒或时区（探针得到 %q）", actual)
		}
		return nil
	})
}

func (w *DaMengWriter) prepareOscarDamengLoad(ctx context.Context, table string, cols, types []string, rows [][]any) error {
	if len(cols) != len(types) {
		return fmt.Errorf("表 %s 缺少神通源列类型", table)
	}
	for i, dt := range types {
		base := source.OscarDamengColumnType(dt)
		fail := func(err error) error { return fmt.Errorf("%s.%s: %w", table, cols[i], err) }
		switch base {
		case "TIME WITH TIME ZONE":
			if err := w.probeOscarDamengTimeZone(ctx); err != nil {
				return fail(err)
			}
		case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE":
			p := oscarDamengLoadTimestampPrecision(rows, i)
			zoned := base == "TIMESTAMP WITH TIME ZONE"
			if p > 6 || zoned {
				if err := w.probeOscarDamengTimestamp(ctx, p, zoned); err != nil {
					return fail(err)
				}
			}
		case "CHAR", `"CHAR"`, "NCHAR", "VARCHAR", "NVARCHAR", "NVARCHAR2", "NAME", "TEXT", "CLOB", "ARRAY", "XMLTYPE":
			if err := w.checkOscarDamengText(ctx, base, rows, i); err != nil {
				return fail(err)
			}
		}
	}
	return nil
}

func oscarDamengLoadTimestampPrecision(rows [][]any, column int) int64 {
	var precision int64
	for _, row := range rows {
		if column >= len(row) {
			continue
		}
		s, ok := row[column].(string)
		if !ok {
			continue
		}
		if _, fraction, found := strings.Cut(s, "."); found {
			end := strings.IndexFunc(fraction, func(r rune) bool { return r < '0' || r > '9' })
			if end >= 0 {
				fraction = fraction[:end]
			}
			precision = max(precision, int64(len(strings.TrimRight(fraction, "0"))))
		}
	}
	return precision
}

func (w *DaMengWriter) checkOscarDamengText(ctx context.Context, base string, rows [][]any, column int) error {
	charset := -1
	for _, row := range rows {
		if column >= len(row) {
			continue
		}
		s, ok := row[column].(string)
		if !ok {
			continue
		}
		if s == "" {
			switch base {
			case "CHAR", `"CHAR"`, "NCHAR", "VARCHAR", "NVARCHAR", "NVARCHAR2", "NAME":
				if err := w.probeOscarDamengEmptyString(ctx); err != nil {
					return err
				}
			}
			continue
		}
		if charset < 0 {
			w.oracleDMMu.Lock()
			value, err := w.damengLoadCharsetLocked(ctx)
			w.oracleDMMu.Unlock()
			if err != nil {
				return err
			}
			charset = value
		}
		if _, err := oracleDMEncodedBytes(s, charset); err != nil {
			return err
		}
	}
	return nil
}
