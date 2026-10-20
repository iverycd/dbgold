package target

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func oracleDMTypeFamily(t string) string {
	t = dmBaseType(strings.ReplaceAll(t, `"`, ""))
	switch t {
	case "NCHAR", "CHARACTER":
		return "CHAR"
	case "NVARCHAR", "NVARCHAR2":
		return "VARCHAR"
	case "TEXT", "LONG", "LONGVARCHAR", "NCLOB":
		return "CLOB"
	case "IMAGE", "LONGVARBINARY":
		return "BLOB"
	case "RAW":
		return "VARBINARY"
	case "SYS.XMLTYPE":
		return "XMLTYPE"
	}
	return t
}

var oracleDMClassID = regexp.MustCompile(`(?i)^CLASS\s*\(?([0-9]+)\)?$`)

func (w *DaMengWriter) validateOracleDMLocalTimestamp(ctx context.Context, p int64) error {
	w.oracleDMMu.Lock()
	defer w.oracleDMMu.Unlock()
	if w.oracleDM.features == nil {
		w.oracleDM.features = map[string]error{}
	}
	key := fmt.Sprintf("existing:localtimestamp:%d", p)
	if err, ok := w.oracleDM.features[key]; ok {
		return err
	}
	err := w.probeOracleDMLocalTimestamp(ctx, p)
	w.oracleDM.features[key] = err
	return err
}

// DM's compatibility view may expose a class as CLASS<id>. Resolve its actual
// schema and name, rather than treating a similarly named user class as native.
func (w *DaMengWriter) validateOracleDMSpecialColumn(ctx context.Context, table, column, want string) error {
	var kind, owner string
	err := w.db.QueryRowContext(ctx, `SELECT DATA_TYPE,NVL(DATA_TYPE_OWNER,'') FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=? AND COLUMN_NAME=?`, w.schema, strings.ToUpper(table), strings.ToUpper(column)).Scan(&kind, &owner)
	if err != nil {
		return fmt.Errorf("无法验证原生类型及其模式: %w", err)
	}
	if m := oracleDMClassID.FindStringSubmatch(strings.TrimSpace(kind)); len(m) == 2 {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return fmt.Errorf("无法识别原生类型 ID: %w", err)
		}
		err = w.db.QueryRowContext(ctx, `SELECT NAME,SF_GET_SCHEMA_NAME_BY_ID(SCHID) FROM SYS.SYSOBJECTS WHERE ID=? AND SUBTYPE$='CLASS'`, id).Scan(&kind, &owner)
		if err != nil {
			return fmt.Errorf("无法解析原生类型 ID: %w", err)
		}
	}
	kind = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(kind, `"`, "")))
	owner = strings.ToUpper(strings.TrimSpace(owner))
	if dot := strings.LastIndexByte(kind, '.'); dot >= 0 {
		if owner != "" && owner != kind[:dot] {
			return fmt.Errorf("原生类型模式信息不一致")
		}
		owner, kind = kind[:dot], kind[dot+1:]
	}
	if want == "XMLTYPE" {
		if kind == "XMLTYPE" && (owner == "" || owner == "SYS") {
			return nil
		}
	} else if owner+"."+kind == want {
		return nil
	}
	return fmt.Errorf("原生类型 %s.%s 与所需 %s 不匹配", owner, kind, want)
}
