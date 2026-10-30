package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbgold/datamigrate/sqlsyntax"
)

func (r *DaMengReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	var method, submethod string
	err := r.db.QueryRowContext(ctx, `SELECT PARTITIONING_TYPE,SUBPARTITIONING_TYPE FROM ALL_PART_TABLES WHERE OWNER=? AND TABLE_NAME=?`, r.schema, table).Scan(&method, &submethod)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取达梦分区目录: %w", err)
	}
	rowMovement, err := catalogOracleStyleRowMovement(ctx, r.db, r.schema, table, true)
	if err != nil {
		return nil, err
	}
	var ddl string
	if err = r.db.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE',?,?)`, table, r.schema).Scan(&ddl); err != nil {
		return nil, fmt.Errorf("读取达梦原生分区定义: %w", err)
	}
	spec, err := catalogOracleStylePartitions(ctx, r.db, r.schema, table, method, submethod, true)
	if err != nil {
		return nil, err
	}
	spec.SourceDDL = ddl
	catalogSetPartitionRowMovement(spec, rowMovement)
	if e := catalogDamengPartitionOptions(spec, ddl); e != nil {
		catalogPartitionUnsupported(spec, e.Error())
	}
	if spec.Automatic != "" && spec.Unsupported == "" {
		if e := catalogDamengInterval(spec, ddl); e != nil {
			catalogPartitionUnsupported(spec, e.Error())
		}
	}
	return spec, nil
}

// DM exposes physical partition storage as UTAB objects. Bind each physical
// table through the native parent IDs; a partition label alone is not identity.
func (r *DaMengReader) PartitionRoots(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT physical.NAME,root.NAME FROM SYS.SYSHPARTTABLEINFO h JOIN SYS.SYSOBJECTS physical ON physical.ID=h.PART_TABLE_ID JOIN SYS.SYSOBJECTS root ON root.ID=h.BASE_TABLE_ID JOIN SYS.SYSOBJECTS owner ON owner.ID=root.SCHID WHERE owner.NAME=? ORDER BY physical.ID`, r.schema)
	if err != nil {
		return nil, fmt.Errorf("读取达梦物理分区父链: %w", err)
	}
	defer rows.Close()
	parents := map[string]string{}
	for rows.Next() {
		var child, parent string
		if err = rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		if child == "" || parent == "" || child == parent {
			return nil, fmt.Errorf("达梦物理分区父链不完整或存在自环")
		}
		if previous, exists := parents[child]; exists && previous != parent {
			return nil, fmt.Errorf("达梦物理分区 %s 的父对象不唯一", child)
		}
		parents[child] = parent
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for child, parent := range parents {
		seen := map[string]bool{child: true}
		for {
			if seen[parent] {
				return nil, fmt.Errorf("达梦物理分区父链存在循环")
			}
			seen[parent] = true
			next, exists := parents[parent]
			if !exists {
				break
			}
			parent = next
		}
		out[child], out[r.schema+"."+child] = parent, parent
	}
	return out, nil
}

func catalogDamengPartitionOptions(spec *PartitionSpec, ddl string) error {
	tokens, err := sqlsyntax.Lex(ddl)
	if err != nil {
		return fmt.Errorf("达梦原生分区定义不能完整解析: %w", err)
	}
	sublevels := 0
	for i := 0; i+2 < len(tokens); i++ {
		if tokens[i].Is("SUBPARTITION") && tokens[i+1].Is("BY") {
			sublevels++
		}
		if tokens[i].Is("VALUES") && tokens[i+1].Is("EQU") {
			return fmt.Errorf("达梦包含上界的原生分区尚不能从兼容目录完整重建")
		}
	}
	if sublevels > 1 {
		return fmt.Errorf("达梦多层子分区尚不能从兼容目录完整重建")
	}
	depth := 0
	for i := 0; i+3 < len(tokens); i++ {
		if tokens[i].Is("(") {
			depth++
		} else if tokens[i].Is(")") {
			depth--
		}
		if depth != 0 || !tokens[i].Is("PARTITION") || !tokens[i+1].Is("BY") || !tokens[i+2].Is("RANGE") || !tokens[i+3].Is("(") {
			continue
		}
		close := catalogMatchingParen(tokens, i+3)
		if close < 0 {
			return fmt.Errorf("达梦分区键括号不完整")
		}
		if close+2 < len(tokens) && tokens[close+1].Is("INTERVAL") && tokens[close+2].Is("(") {
			end := catalogMatchingParen(tokens, close+2)
			if end < 0 {
				return fmt.Errorf("达梦间隔分区表达式不完整")
			}
			var raw []string
			for _, token := range tokens[close+3 : end] {
				raw = append(raw, token.Text)
			}
			spec.Automatic = strings.Join(raw, " ")
			if spec.Automatic == "" {
				return fmt.Errorf("达梦间隔分区表达式为空")
			}
		}
		return nil
	}
	return nil
}

func catalogMatchingParen(tokens []sqlsyntax.Token, open int) int {
	depth := 0
	for i := open; i < len(tokens); i++ {
		if tokens[i].Is("(") {
			depth++
		} else if tokens[i].Is(")") {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (r *DaMengReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	return catalogOracleStyleIndexes(ctx, r.db, r.schema, table, true)
}
