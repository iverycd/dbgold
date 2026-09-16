package target

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

// MySQLTextRules describes an effective target collation, never a source mapping.
type MySQLTextRules struct {
	Charset   string
	Collation string
	MaxBytes  int
	PadSpace  bool
}

var mysqlTextName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func (p MySQLProfile) TextRules() MySQLTextRules {
	return MySQLTextRules{p.TargetCharset, p.TargetCollation, p.CharsetMaxBytes, p.CollationPadSpace}
}

// Names interpolated into SQL are validated after reading the server catalog.
func (r MySQLTextRules) Validate() error {
	if !mysqlTextName.MatchString(r.Charset) || !mysqlTextName.MatchString(r.Collation) || r.MaxBytes < 1 {
		return fmt.Errorf("目标字符集/排序规则元数据无效: %q/%q", r.Charset, r.Collation)
	}
	return nil
}

func ReadMySQLDatabaseTextRules(ctx context.Context, db *sql.DB, database string) (MySQLTextRules, error) {
	var charset, collation string
	err := db.QueryRowContext(ctx, `SELECT DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?`, database).Scan(&charset, &collation)
	if err != nil {
		return MySQLTextRules{}, fmt.Errorf("读取目标 database 默认字符配置: %w", err)
	}
	r, err := ReadMySQLTextRules(ctx, db, collation)
	if err == nil && r.Charset != charset {
		err = fmt.Errorf("目标 database 字符配置不一致，请重新预检")
	}
	return r, err
}

func ReadMySQLTextRules(ctx context.Context, db *sql.DB, collation string) (MySQLTextRules, error) {
	r := MySQLTextRules{Collation: collation}
	err := db.QueryRowContext(ctx, `SELECT c.CHARACTER_SET_NAME,s.MAXLEN FROM information_schema.COLLATIONS c JOIN information_schema.CHARACTER_SETS s ON s.CHARACTER_SET_NAME=c.CHARACTER_SET_NAME WHERE c.COLLATION_NAME=?`, collation).Scan(&r.Charset, &r.MaxBytes)
	if err != nil {
		return r, fmt.Errorf("读取目标排序规则 %s: %w", collation, err)
	}
	if err = r.Validate(); err != nil {
		return r, err
	}
	// Direct comparison works on both 5.7 and 8.0 (5.7 has no PAD_ATTRIBUTE).
	q := "SELECT (CONVERT(_utf8mb4 X'61' USING " + r.Charset + ") COLLATE " + r.Collation + " = CONVERT(_utf8mb4 X'6120' USING " + r.Charset + ") COLLATE " + r.Collation + ")"
	err = db.QueryRowContext(ctx, q).Scan(&r.PadSpace)
	return r, err
}
