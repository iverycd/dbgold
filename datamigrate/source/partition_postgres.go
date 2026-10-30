package source

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

func (r *PostgresReader) partitionProfile(ctx context.Context) error {
	if r.profile.Catalog != "" && r.profile.Fields != nil {
		return nil
	}
	var last error
	for _, catalog := range []string{"pg_catalog", "sys_catalog"} {
		query := `SELECT c.relname,a.attname FROM ` + QuotePGIdent(catalog) + `.pg_attribute a JOIN ` + QuotePGIdent(catalog) + `.pg_class c ON c.oid=a.attrelid JOIN ` + QuotePGIdent(catalog) + `.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname IN ('pg_class','pg_partitioned_table','pg_partition','pg_inherits') AND a.attnum>0 AND NOT a.attisdropped`
		rows, err := r.db.QueryContext(ctx, query, catalog)
		if err != nil {
			last = err
			continue
		}
		fields := map[string]bool{}
		for rows.Next() {
			var relation, field string
			if err = rows.Scan(&relation, &field); err != nil {
				break
			}
			fields[relation+"."+field] = true
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if !fields["pg_class.relkind"] {
			last = fmt.Errorf("目录缺少 pg_class.relkind")
			continue
		}
		r.profile.Catalog, r.profile.Fields = catalog, fields
		return nil
	}
	return fmt.Errorf("无法识别分区目录: %w", last)
}

func (r *PostgresReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	if err := r.partitionProfile(ctx); err != nil {
		return nil, err
	}
	return r.pgPartitionMetadata(ctx, r.schema, table, map[string]bool{})
}

func (r *PostgresReader) pgPartitionMetadata(ctx context.Context, schema, table string, visiting map[string]bool) (*PartitionSpec, error) {
	id := schema + "\x00" + table
	if visiting[id] {
		return nil, fmt.Errorf("分区父链存在循环: %s.%s", schema, table)
	}
	visiting[id] = true
	defer delete(visiting, id)
	var oid int64
	var kind string
	native := "false"
	if r.profile.Fields["pg_class.parttype"] {
		native = "c.parttype IN ('p','s')"
	}
	if r.profile.Fields["pg_partition.parentid"] {
		native += " OR EXISTS(SELECT 1 FROM " + r.qcat("pg_partition") + " part WHERE part.parentid=c.oid)"
	}
	var nativePartition, inherited bool
	query := `SELECT c.oid,c.relkind,(` + native + `),EXISTS(SELECT 1 FROM ` + r.qcat("pg_inherits") + ` h WHERE h.inhparent=c.oid OR h.inhrelid=c.oid) FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p','f','m')`
	var rowMovement bool
	values := []interface{}{&oid, &kind, &nativePartition, &inherited}
	if r.profile.Fields["pg_class.relrowmovement"] {
		query = strings.Replace(query, " FROM "+r.qcat("pg_class")+" c JOIN ", ",c.relrowmovement FROM "+r.qcat("pg_class")+" c JOIN ", 1)
		values = append(values, &rowMovement)
	}
	if err := r.db.QueryRowContext(ctx, query, schema, table).Scan(values...); err != nil {
		return nil, err
	}
	if kind != "p" {
		if nativePartition {
			spec, err := r.pgNativePartitionMetadata(ctx, oid, schema)
			if err == nil && spec != nil {
				if !r.profile.Fields["pg_class.relrowmovement"] {
					catalogPartitionUnsupported(spec, "原生分区目录缺少可确认跨分区 UPDATE 行移动规则的 pg_class.relrowmovement")
				} else if rowMovement {
					spec.RowMovement = "enabled"
				} else {
					spec.RowMovement = "disabled"
				}
			}
			return spec, err
		}
		if inherited {
			return &PartitionSpec{Unsupported: "普通表继承或独立选择的分区子表不能作为逻辑根表迁移"}, nil
		}
		return nil, nil
	}
	if !r.profile.Fields["pg_partitioned_table.partstrat"] || !r.profile.Fields["pg_class.relpartbound"] {
		return &PartitionSpec{Unsupported: "源库缺少完整声明式分区键及边界目录"}, nil
	}
	var strategy, attributes, keyDefinition string
	query = `SELECT p.partstrat,p.partattrs::text,pg_get_partkeydef(p.partrelid) FROM ` + r.qcat("pg_partitioned_table") + ` p WHERE p.partrelid=$1`
	if err := r.db.QueryRowContext(ctx, query, oid).Scan(&strategy, &attributes, &keyDefinition); err != nil {
		return nil, err
	}
	method := map[string]string{"r": "RANGE", "l": "LIST", "h": "HASH"}[strategy]
	spec := &PartitionSpec{Method: method, SourceDDL: keyDefinition, RowMovement: "enabled"}
	if strings.Contains(r.profile.Version, "PostgreSQL 10.") {
		spec.RowMovement = "disabled"
	}
	if method == "" {
		spec.Unsupported = "未知 PostgreSQL 分区方法 " + strategy
		return spec, nil
	}
	if method == "HASH" {
		spec.Algorithm = "postgres-hash"
		if r.profile.Product != "" && r.profile.Product != "postgres" {
			spec.Algorithm = r.profile.Product + "-hash"
		}
	}
	keys, err := pgPartitionKeys(keyDefinition)
	if err != nil {
		spec.Unsupported = err.Error()
		return spec, nil
	}
	spec.Keys = keys
	if err := r.pgPartitionExpressionDependencies(ctx, oid, attributes, spec); err != nil {
		return nil, err
	}
	query = `SELECT a.attname,format_type(a.atttypid,a.atttypmod),COALESCE(coll.collname,'') FROM ` + r.qcat("pg_attribute") + ` a LEFT JOIN ` + r.qcat("pg_collation") + ` coll ON coll.oid=a.attcollation WHERE a.attrelid=$1 AND a.attnum=ANY(SELECT unnest(partattrs) FROM ` + r.qcat("pg_partitioned_table") + ` WHERE partrelid=$1)`
	rows, err := r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, typ, collation string
		if err = rows.Scan(&name, &typ, &collation); err != nil {
			break
		}
		for i := range spec.Keys {
			if spec.Keys[i].Column == name {
				spec.Keys[i].DataType = typ
				spec.Keys[i].Collation = collation
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if r.profile.Fields["pg_partitioned_table.partclass"] {
		query = `SELECT k.position,n.nspname,o.opcname,o.opcdefault,format_type(o.opcintype,NULL) FROM ` + r.qcat("pg_partitioned_table") + ` p CROSS JOIN LATERAL unnest(p.partclass::oid[]) WITH ORDINALITY AS k(opcoid,position) JOIN ` + r.qcat("pg_opclass") + ` o ON o.oid=k.opcoid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=o.opcnamespace WHERE p.partrelid=$1 ORDER BY k.position`
		classes, e := r.db.QueryContext(ctx, query, oid)
		if e != nil {
			return nil, e
		}
		count := 0
		for classes.Next() {
			var position int
			var owner, name, typ string
			var standard bool
			if e = classes.Scan(&position, &owner, &name, &standard, &typ); e != nil {
				break
			}
			if position != count+1 || position > len(spec.Keys) {
				e = fmt.Errorf("分区运算符类的键顺序不完整")
				break
			}
			count++
			if owner != "pg_catalog" || !standard {
				catalogPartitionUnsupported(spec, "非默认或用户定义分区运算符类 "+owner+"."+name+" 的路由等价适配尚未实现")
			}
			if spec.Keys[position-1].Expression != "" {
				spec.Keys[position-1].DataType = typ
			}
		}
		if e == nil {
			e = classes.Err()
		}
		closeErr := classes.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return nil, e
		}
		if count != len(spec.Keys) {
			return nil, fmt.Errorf("分区运算符类的键数量与目录不符")
		}
	}
	query = `SELECT child.oid,ns.nspname,child.relname,child.relkind,pg_get_expr(child.relpartbound,child.oid) FROM ` + r.qcat("pg_inherits") + ` h JOIN ` + r.qcat("pg_class") + ` child ON child.oid=h.inhrelid JOIN ` + r.qcat("pg_namespace") + ` ns ON ns.oid=child.relnamespace WHERE h.inhparent=$1 ORDER BY child.oid`
	rows, err = r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	type childRow struct {
		oid                       int64
		schema, name, kind, bound string
	}
	var children []childRow
	for rows.Next() {
		var child childRow
		if err = rows.Scan(&child.oid, &child.schema, &child.name, &child.kind, &child.bound); err != nil {
			break
		}
		children = append(children, child)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr = rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	for i, child := range children {
		part := Partition{Schema: child.schema, Name: child.name, Ordinal: i + 1}
		if e := pgPartitionBound(child.bound, method, spec.Keys, &part); e != nil {
			spec.Unsupported = fmt.Sprintf("分区 %s.%s: %v", child.schema, child.name, e)
		}
		if child.kind == "p" {
			part.Subpartition, err = r.pgPartitionMetadata(ctx, child.schema, child.name, visiting)
			if err != nil {
				return nil, err
			}
		}
		spec.Partitions = append(spec.Partitions, part)
	}
	// Empty partitioned roots are legal, but cannot receive data until a leaf exists.
	return spec, nil
}

func (r *PostgresReader) PartitionRoots(ctx context.Context) (map[string]string, error) {
	if err := r.partitionProfile(ctx); err != nil {
		return nil, err
	}
	result := map[string]string{}
	if !r.profile.Fields["pg_class.relispartition"] {
		return result, nil
	}
	query := `WITH RECURSIVE roots AS (SELECT c.oid,c.relname root,c.oid node FROM ` + r.qcat("pg_class") + ` c JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='p' AND NOT c.relispartition UNION ALL SELECT roots.oid,roots.root,h.inhrelid FROM roots JOIN ` + r.qcat("pg_inherits") + ` h ON h.inhparent=roots.node) SELECT ns.nspname,c.relname,roots.root FROM roots JOIN ` + r.qcat("pg_class") + ` c ON c.oid=roots.node JOIN ` + r.qcat("pg_namespace") + ` ns ON ns.oid=c.relnamespace WHERE roots.node<>roots.oid ORDER BY roots.root,ns.nspname,c.relname`
	rows, err := r.db.QueryContext(ctx, query, r.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, leaf, root string
		if err := rows.Scan(&schema, &leaf, &root); err != nil {
			return nil, err
		}
		result[schema+"."+leaf] = root
		if schema == r.schema {
			result[leaf] = root
		}
	}
	return result, rows.Err()
}

func pgPartitionKeys(definition string) ([]PartitionKey, error) {
	tokens, err := sqlsyntax.Lex(definition)
	if err != nil {
		return nil, err
	}
	if len(tokens) < 3 || !tokens[1].Is("(") || !tokens[len(tokens)-1].Is(")") {
		return nil, fmt.Errorf("无法解析分区键定义")
	}
	groups, err := pgPartitionTokenGroups(tokens[2 : len(tokens)-1])
	if err != nil {
		return nil, err
	}
	var keys []PartitionKey
	for _, group := range groups {
		if len(group) == 1 && (group[0].Kind == 'q' || group[0].Kind == 'i') {
			name := group[0].Value
			if group[0].Kind != 'q' {
				name = strings.ToLower(name)
			}
			keys = append(keys, PartitionKey{Column: name})
		} else {
			var raw []string
			for _, token := range group {
				raw = append(raw, token.Text)
			}
			keys = append(keys, PartitionKey{Expression: strings.Join(raw, " ")})
		}
	}
	return keys, nil
}

func pgPartitionTokenGroups(tokens []sqlsyntax.Token) ([][]sqlsyntax.Token, error) {
	depth := 0
	start := 0
	var groups [][]sqlsyntax.Token
	for i, token := range tokens {
		if token.Is("(") {
			depth++
		}
		if token.Is(")") {
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("分区表达式括号不平衡")
			}
		}
		if token.Is(",") && depth == 0 {
			if start == i {
				return nil, fmt.Errorf("空分区表达式")
			}
			groups = append(groups, tokens[start:i])
			start = i + 1
		}
	}
	if depth != 0 || start == len(tokens) {
		return nil, fmt.Errorf("空或不完整分区表达式")
	}
	groups = append(groups, tokens[start:])
	return groups, nil
}

func pgPartitionBound(raw, method string, keys []PartitionKey, part *Partition) error {
	tokens, err := sqlsyntax.Lex(raw)
	if err != nil {
		return err
	}
	if len(tokens) == 1 && tokens[0].Is("DEFAULT") {
		part.Default = true
		return nil
	}
	if len(tokens) < 4 || !tokens[0].Is("FOR") || !tokens[1].Is("VALUES") {
		return fmt.Errorf("无法解析原生分区边界")
	}
	if method == "HASH" {
		if len(tokens) != 10 || !tokens[2].Is("WITH") || !tokens[3].Is("(") || !tokens[4].Is("MODULUS") || !tokens[6].Is(",") || !tokens[7].Is("REMAINDER") || !tokens[9].Is(")") {
			return fmt.Errorf("HASH 原生边界格式未知")
		}
		part.HashModulus, err = strconv.Atoi(tokens[5].Value)
		if err != nil {
			return err
		}
		part.HashRemainder, err = strconv.Atoi(tokens[8].Value)
		if err == nil && (part.HashModulus <= 0 || part.HashRemainder < 0 || part.HashRemainder >= part.HashModulus) {
			err = fmt.Errorf("HASH 模数或余数超出合法范围")
		}
		return err
	}
	readTuple := func(start int) ([]PartitionValue, int, error) {
		if start >= len(tokens) || !tokens[start].Is("(") {
			return nil, start, fmt.Errorf("缺少分区值括号")
		}
		depth := 0
		end := -1
		for i := start; i < len(tokens); i++ {
			if tokens[i].Is("(") {
				depth++
			}
			if tokens[i].Is(")") {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end < 0 {
			return nil, start, fmt.Errorf("未闭合分区值")
		}
		groups, e := pgPartitionTokenGroups(tokens[start+1 : end])
		if e != nil {
			return nil, start, e
		}
		var values []PartitionValue
		for i, group := range groups {
			typ := ""
			if method == "LIST" && len(keys) > 0 {
				typ = keys[0].DataType
			} else if i < len(keys) {
				typ = keys[i].DataType
			}
			value, e := pgPartitionLiteral(group, typ)
			if e != nil {
				return nil, start, e
			}
			values = append(values, value)
		}
		return values, end + 1, nil
	}
	if method == "RANGE" {
		if !tokens[2].Is("FROM") {
			return fmt.Errorf("RANGE 缺少 FROM")
		}
		lower, next, e := readTuple(3)
		if e != nil {
			return e
		}
		if next >= len(tokens) || !tokens[next].Is("TO") {
			return fmt.Errorf("RANGE 缺少 TO")
		}
		upper, end, e := readTuple(next + 1)
		if e != nil {
			return e
		}
		if end != len(tokens) || len(lower) != len(keys) || len(upper) != len(keys) {
			return fmt.Errorf("RANGE 分区键数量或边界格式不同")
		}
		part.Lower, part.Upper, part.LowerInclusive = lower, upper, true
		return nil
	}
	if method == "LIST" {
		if len(keys) != 1 || !tokens[2].Is("IN") {
			return fmt.Errorf("当前源目录仅支持完整单键 LIST 值")
		}
		values, end, e := readTuple(3)
		if e != nil {
			return e
		}
		if end != len(tokens) {
			return fmt.Errorf("LIST 边界尾部未知")
		}
		for _, value := range values {
			part.Values = append(part.Values, []PartitionValue{value})
		}
		return nil
	}
	return fmt.Errorf("未知分区方法 %s", method)
}

func pgPartitionLiteral(tokens []sqlsyntax.Token, typ string) (PartitionValue, error) {
	// pg_get_expr may append an explicit native type cast to scalar literals.
	for i, token := range tokens {
		if token.Is("::") {
			if i == 0 || i+1 == len(tokens) {
				return PartitionValue{}, fmt.Errorf("分区常量类型转换不完整")
			}
			for _, suffix := range tokens[i+1:] {
				if suffix.Kind != 'i' && suffix.Kind != 'q' && suffix.Kind != 'n' && !suffix.Is(".") && !suffix.Is("(") && !suffix.Is(")") && !suffix.Is(",") {
					return PartitionValue{}, fmt.Errorf("分区常量类型转换包含未知表达式")
				}
			}
			if typ == "" {
				// Expression keys have no attribute type; native boundary casts
				// still identify their exact scalar type without evaluating SQL.
				for _, suffix := range tokens[i+1:] {
					if suffix.Kind != 'i' && suffix.Kind != 'q' {
						break
					}
					typ = suffix.Value
					break
				}
			}
			tokens = tokens[:i]
			break
		}
	}
	if len(tokens) == 1 {
		token := tokens[0]
		for _, kind := range []string{"NULL", "MINVALUE", "MAXVALUE"} {
			if token.Is(kind) {
				return PartitionValue{Kind: strings.ToLower(kind)}, nil
			}
		}
		if token.Kind == 's' {
			kind := "string"
			upper := strings.ToUpper(typ)
			switch upper {
			case "SMALLINT", "INTEGER", "BIGINT", "INT2", "INT4", "INT8", "NUMERIC", "DECIMAL":
				value, err := catalogPartitionValue(token.Value)
				if err != nil || value.Kind != "number" {
					return PartitionValue{}, fmt.Errorf("数值分区边界不是精确数字: %q", token.Value)
				}
				return value, nil
			}
			if strings.HasPrefix(upper, "TIMESTAMP") {
				kind = "timestamp"
			} else if upper == "DATE" {
				kind = "date"
			}
			return PartitionValue{Kind: kind, Text: token.Value}, nil
		}
		if token.Kind == 'n' {
			return PartitionValue{Kind: "number", Text: token.Text}, nil
		}
	}
	if len(tokens) == 2 && (tokens[0].Is("-") || tokens[0].Is("+")) && tokens[1].Kind == 'n' {
		return PartitionValue{Kind: "number", Text: tokens[0].Text + tokens[1].Text}, nil
	}
	return PartitionValue{}, fmt.Errorf("分区边界不是可证明的原生常量")
}

func (r *PostgresReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	if err := r.partitionProfile(ctx); err != nil {
		return nil, err
	}
	if r.profile.Fields["pg_partition.partstrategy"] && !r.profile.Fields["pg_partitioned_table.partstrat"] {
		return r.pgNativePartitionIndexes(ctx, table)
	}
	query := `SELECT i.relname,x.indisunique,x.indisprimary,a.attname,parent.relname FROM ` + r.qcat("pg_class") + ` t JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace JOIN ` + r.qcat("pg_index") + ` x ON x.indrelid=t.oid JOIN ` + r.qcat("pg_class") + ` i ON i.oid=x.indexrelid JOIN ` + r.qcat("pg_attribute") + ` a ON a.attrelid=t.oid AND a.attnum=ANY(x.indkey) LEFT JOIN ` + r.qcat("pg_inherits") + ` h ON h.inhrelid=i.oid LEFT JOIN ` + r.qcat("pg_class") + ` parent ON parent.oid=h.inhparent WHERE n.nspname=$1 AND t.relname=$2 ORDER BY i.relname,array_position(x.indkey::smallint[],a.attnum)`
	rows, err := r.db.QueryContext(ctx, query, r.schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IndexInfo
	positions := map[string]int{}
	for rows.Next() {
		var name, column string
		var unique, primary bool
		var parent sql.NullString
		if err := rows.Scan(&name, &unique, &primary, &column, &parent); err != nil {
			return nil, err
		}
		at, ok := positions[name]
		if !ok {
			at = len(result)
			positions[name] = at
			result = append(result, IndexInfo{TableName: table, IndexName: name, IsUnique: unique, IsPrimary: primary, Locality: "GLOBAL", Derived: parent.Valid, ParentIndex: parent.String})
		}
		result[at].Columns = append(result[at].Columns, column)
	}
	return result, rows.Err()
}
