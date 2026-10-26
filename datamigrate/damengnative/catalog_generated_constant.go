package damengnative

import (
	"database/sql"
	"fmt"
)

// GET_DDL reparses generated expressions and adds grouping parentheses around
// arithmetic. Persist a typed scalar literal instead of that arithmetic, and
// prove the native conversion is exact before it enters a frozen recipe. Only
// native constants enter this query; application routines are never evaluated.
func (r *catalogReader) freezeNativeGeneratedConstant(c Column, expression string) (string, error) {
	if !materializedEmptyPartitionConstant(expression) {
		return "", fmt.Errorf("生成分区键的只读种子不是原生常量")
	}
	read := c
	read.Name = "DBGOLD_GENERATED_KEY"
	var raw sql.NullString
	query := "SELECT " + materializedEmptyClusterRead(read) + " FROM (SELECT " + expression + " AS " + Quote(read.Name) + " FROM DUAL)"
	if err := r.db.QueryRowContext(r.ctx, query).Scan(&raw); err != nil {
		return "", err
	}
	if !raw.Valid {
		return "", fmt.Errorf("生成分区键的冻结种子意外为空")
	}
	literal, err := materializedEmptyClusterValue(c, &raw.String)
	if err != nil {
		return "", err
	}
	var equal int
	if err = r.db.QueryRowContext(r.ctx, "SELECT CASE WHEN "+literal+"="+expression+" THEN 1 ELSE 0 END FROM DUAL").Scan(&equal); err != nil {
		return "", err
	}
	if equal != 1 {
		return "", fmt.Errorf("生成分区键的原生文本转换不能无损重建种子")
	}
	return literal, nil
}
