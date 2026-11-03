package target

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dbgold/datamigrate/failure"
)

// This capability installs reviewed application routines, not arbitrary source
// SQL. blockedTables uses destination identifiers and prevents binding a new
// function to stale tables left behind by a failed migration.
// The historical MySQL-prefixed optional API is retained for compatibility;
// all supported sources targeting Dameng now use the same reviewed routines.
type MySQLDamengBusinessFunctionInitializer interface {
	EnsureMySQLDamengBusinessFunctions(context.Context, map[string]string) (*CompatibilityFunctionsReport, error)
}

type dmBusinessDependency struct {
	table   string
	columns []string
}

type dmBusinessFunction struct {
	name, parameters, result, body string
	dependencies                   []dmBusinessDependency
}

func dmBusinessFunctions() []dmBusinessFunction {
	codes := []dmBusinessDependency{{"CODE_MAIN", []string{"CODEID", "CODENAME"}}, {"CODE_ITEMS", []string{"CODEID", "ITEMVALUE", "ITEMTEXT"}}}
	lookup := func(query string) string {
		return "  v_result VARCHAR(50);\nBEGIN\n  " + query + ";\n  RETURN v_result;\nEXCEPTION\n  WHEN NO_DATA_FOUND THEN RETURN NULL;\nEND;"
	}
	return []dmBusinessFunction{
		{"GETCHILDOULIST", "p_rootid IN VARCHAR(50)", "CLOB", dmChildOUListBody,
			[]dmBusinessDependency{{"FRAME_OU", []string{"OUGUID", "PARENTOUGUID"}}}},
		{"GETITEMTEXT", "p_question IN VARCHAR(50), p_codename IN VARCHAR(50)", "VARCHAR(50)", lookup(`SELECT b."ITEMTEXT" INTO v_result FROM {{CODE_MAIN}} a JOIN {{CODE_ITEMS}} b ON a."CODEID"=b."CODEID" WHERE a."CODENAME"=p_codename AND b."ITEMVALUE"=p_question`), codes},
		{"GETOUNAMEBYUSERGUID", "p_userguid IN VARCHAR(50)", "VARCHAR(50)", lookup(`SELECT a."OUNAME" INTO v_result FROM {{FRAME_OU}} a JOIN {{FRAME_USER}} b ON a."OUGUID"=b."OUGUID" WHERE b."USERGUID"=p_userguid`),
			[]dmBusinessDependency{{"FRAME_OU", []string{"OUGUID", "OUNAME"}}, {"FRAME_USER", []string{"OUGUID", "USERGUID"}}}},
		{"GETQUESTIONTEXT", "p_question IN VARCHAR(50)", "VARCHAR(50)", lookup(`SELECT b."ITEMTEXT" INTO v_result FROM {{CODE_MAIN}} a JOIN {{CODE_ITEMS}} b ON a."CODEID"=b."CODEID" WHERE a."CODENAME"='涉及问题' AND b."ITEMVALUE"=p_question`), codes},
		{"GETUSERNAMEBYUSERGUID", "p_userguid IN VARCHAR(50)", "VARCHAR(50)", lookup(`SELECT u."DISPLAYNAME" INTO v_result FROM {{FRAME_USER}} u WHERE u."USERGUID"=p_userguid`),
			[]dmBusinessDependency{{"FRAME_USER", []string{"USERGUID", "DISPLAYNAME"}}}},
		{"SELECTALLUSERCOUNT", "", "VARCHAR(100)", "  v_count BIGINT;\nBEGIN\n  SELECT COUNT(*) INTO v_count FROM {{FRAME_USER}};\n  RETURN 'mysql count :' || CAST(v_count AS VARCHAR(30));\nEND;",
			[]dmBusinessDependency{{"FRAME_USER", nil}}},
		{"FUN_GETPY", "p_name IN VARCHAR2", "VARCHAR2", dmFunGetPyBody, nil},
	}
}

// All growing strings stay CLOB, including the membership haystack. A DAG can
// traverse at most COUNT(*) edges: a further nonempty frontier proves a cycle.
// Unlike a global visited set this also accepts overlapping roots and branches.
// Source GROUP_CONCAT has no ordering; use a stable order within each level.
// DM references: pm/check-phrases.html (LISTAGG2), pm/dbms_lob-package.html.
const dmChildOUListBody = `  v_result CLOB;
  v_frontier CLOB;
  v_next CLOB;
  v_members CLOB;
  v_depth BIGINT := 0;
  v_max_depth BIGINT;
BEGIN
  IF p_rootid IS NULL OR LENGTH(p_rootid)=0 THEN
    RETURN TO_CLOB(p_rootid);
  END IF;
  v_result := TO_CLOB(p_rootid);
  v_frontier := TO_CLOB(p_rootid);
  SELECT COUNT(*) INTO v_max_depth FROM {{FRAME_OU}};
  WHILE v_frontier IS NOT NULL LOOP
    v_members := TO_CLOB(',') || v_frontier || TO_CLOB(',');
    SELECT LISTAGG2(t."OUGUID", ',') WITHIN GROUP (ORDER BY t."OUGUID")
      INTO v_next FROM {{FRAME_OU}} t
      WHERE t."OUGUID" IS NOT NULL AND t."PARENTOUGUID" IS NOT NULL
        AND INSTR(t."PARENTOUGUID", ',')=0
        AND DBMS_LOB.INSTR(v_members, ',' || t."PARENTOUGUID" || ',')>0;
    IF v_next IS NULL OR DBMS_LOB.GETLENGTH(v_next)=0 THEN
      EXIT;
    END IF;
    v_depth := v_depth+1;
    IF v_depth>v_max_depth THEN
      RAISE_APPLICATION_ERROR(-20001, 'getChildOuList: cycle detected in FRAME_OU');
    END IF;
    v_result := v_result || TO_CLOB(',') || v_next;
    v_frontier := v_next;
  END LOOP;
  RETURN v_result;
END;`

func (w *DaMengWriter) dmBusinessFunctionDDL(spec dmBusinessFunction) string {
	body := spec.body
	for _, dep := range spec.dependencies {
		body = strings.ReplaceAll(body, "{{"+dep.table+"}}", w.qualifiedTable(dep.table))
	}
	params := ""
	if spec.parameters != "" {
		params = "(" + spec.parameters + ")"
	}
	return "CREATE FUNCTION " + w.qualifiedTable(spec.name) + params + "\nRETURN " + spec.result + "\nAS\n" + body
}

func (w *DaMengWriter) EnsureMySQLDamengBusinessFunctions(ctx context.Context, blockedTables map[string]string) (*CompatibilityFunctionsReport, error) {
	specs := dmBusinessFunctions()
	report := &CompatibilityFunctionsReport{}
	var errs []error
	var stopped error
	for _, spec := range specs {
		item := CompatibilityFunctionResult{Name: spec.name, Kind: "function", Schema: w.schema,
			Signature: spec.name + "(" + spec.parameters + ")", Status: "failed", DDL: w.dmBusinessFunctionDDL(spec)}
		if ctx.Err() != nil {
			stopped = ctx.Err()
		}
		var err error
		if stopped != nil {
			err = fmt.Errorf("任务已终止，未创建函数: %w", stopped)
		} else {
			err = w.ensureMySQLDamengBusinessFunction(ctx, spec, blockedTables, &item)
		}
		if err != nil {
			item.Error = err.Error()
			errs = append(errs, err)
			if failure.IsTaskError(err) {
				stopped = err
			}
		}
		report.Items = append(report.Items, item)
	}
	report.finish(nil)
	return report, errors.Join(errs...)
}

func (w *DaMengWriter) ensureMySQLDamengBusinessFunction(ctx context.Context, spec dmBusinessFunction, blocked map[string]string, item *CompatibilityFunctionResult) error {
	var exists int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='FUNCTION'`, w.schema, spec.name).Scan(&exists); err != nil {
		return fmt.Errorf("检查同名函数: %w", err)
	}
	if exists > 0 {
		item.Status, item.DDL = "reused", ""
		item.DefinitionWarning = "已存在，跳过，未校验定义"
		return nil
	}
	for _, dep := range spec.dependencies {
		if reason := blocked[dep.table]; reason != "" {
			return fmt.Errorf("依赖表 %s.%s 迁移失败，未创建函数: %s", w.schema, dep.table, reason)
		}
		if err := w.checkDMBusinessDependency(ctx, dep); err != nil {
			return err
		}
	}
	// DMSQL is one execution unit. No DELIMITER, client slash, or splitting.
	if _, err := w.db.ExecContext(ctx, item.DDL); err != nil {
		return fmt.Errorf("创建函数 %s: %w", spec.name, err)
	}
	var status string
	if err := w.db.QueryRowContext(ctx, `SELECT STATUS FROM ALL_OBJECTS WHERE OWNER=? AND OBJECT_NAME=? AND OBJECT_TYPE='FUNCTION'`, w.schema, spec.name).Scan(&status); err != nil {
		return fmt.Errorf("检查函数编译状态: %w", err)
	}
	if strings.TrimSpace(status) != "VALID" {
		rows, err := w.db.QueryContext(ctx, `SELECT LINE,POSITION,TEXT FROM ALL_ERRORS WHERE OWNER=? AND NAME=? AND TYPE='FUNCTION' ORDER BY SEQUENCE`, w.schema, spec.name)
		if err != nil {
			return fmt.Errorf("函数编译状态 %s，读取编译错误失败: %w", status, err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var line, pos int
			var detail string
			if err := rows.Scan(&line, &pos, &detail); err != nil {
				return err
			}
			details = append(details, fmt.Sprintf("%d:%d %s", line, pos, detail))
		}
		return errors.Join(fmt.Errorf("函数编译状态 %s: %s", status, strings.Join(details, "; ")), rows.Err())
	}
	item.Status = "created"
	return nil
}

func (w *DaMengWriter) checkDMBusinessDependency(ctx context.Context, dep dmBusinessDependency) error {
	rows, err := w.db.QueryContext(ctx, `SELECT COLUMN_NAME FROM ALL_TAB_COLUMNS WHERE OWNER=? AND TABLE_NAME=?`, w.schema, dep.table)
	if err != nil {
		return fmt.Errorf("读取函数依赖 %s.%s: %w", w.schema, dep.table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("函数依赖表 %s.%s 不存在或不可见", w.schema, dep.table)
	}
	for _, column := range dep.columns {
		if !columns[column] {
			return fmt.Errorf("函数依赖列 %s.%s.%s 不存在或不可见", w.schema, dep.table, column)
		}
	}
	return nil
}
