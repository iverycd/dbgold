package target

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/source"
)

// Use catalogs available on older PG kernels, rather than pg_sequence or
// CREATE SEQUENCE IF NOT EXISTS. Both automatic and identity dependencies
// must point to the intended column. OWNED BY NONE alone is insufficient:
// defaults on other columns can still reference the sequence in reverse.
const pgSequenceReuseSQL = `SELECT c.relkind, NOT EXISTS (
 SELECT 1 FROM pg_catalog.pg_depend dep
 LEFT JOIN pg_catalog.pg_class t ON t.oid=dep.refobjid
 LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid=t.relnamespace
 LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=dep.refobjsubid AND NOT a.attisdropped
 WHERE dep.classid='pg_catalog.pg_class'::regclass AND dep.objid=c.oid
 AND dep.refclassid='pg_catalog.pg_class'::regclass AND dep.deptype IN ('a','i')
 AND (tn.nspname IS DISTINCT FROM $1 OR t.relname IS DISTINCT FROM $3 OR a.attname IS DISTINCT FROM $4)
) AND NOT EXISTS (
 SELECT 1 FROM pg_catalog.pg_depend dep
 LEFT JOIN pg_catalog.pg_attrdef ad ON ad.oid=dep.objid
 LEFT JOIN pg_catalog.pg_class t ON t.oid=ad.adrelid
 LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid=t.relnamespace
 LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=ad.adnum AND NOT a.attisdropped
 WHERE dep.classid='pg_catalog.pg_attrdef'::regclass
 AND dep.refclassid='pg_catalog.pg_class'::regclass AND dep.refobjid=c.oid
 AND (tn.nspname IS DISTINCT FROM $1 OR t.relname IS DISTINCT FROM $3 OR a.attname IS DISTINCT FROM $4)
) FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname=$1 AND c.relname=$2`

// createPGSequence keeps each statement independently observable. In particular,
// a partially completed binding can be retried without resetting sequence state.
func createPGSequence(ctx context.Context, db *sql.DB, d dialect.Dialect, schema string, seq source.SequenceInfo, owner string) error {
	statements := d.SequenceStatements(schema, seq, owner)
	var verificationErr error
	verifyDuplicate := func(createErr error) bool {
		code := compatibilitySQLState(createErr)
		if code != "42P07" && code != "42710" {
			return false
		}
		verificationErr = inspectPGSequenceReuse(ctx, db, d, schema, seq)
		return verificationErr == nil
	}
	for i, statement := range statements {
		var err error
		if i == 0 {
			_, err = execObservedAllow(ctx, db, statement.SQL, verifyDuplicate)
		} else {
			_, err = execObserved(ctx, db, statement.SQL)
		}
		if err != nil {
			return errors.Join(err, verificationErr)
		}
	}
	return nil
}

func inspectPGSequenceReuse(ctx context.Context, db *sql.DB, d dialect.Dialect, schema string, seq source.SequenceInfo) error {
	if schema == "" {
		var current sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return fmt.Errorf("核验重复序列的目标 schema: %w", err)
		}
		if !current.Valid || current.String == "" {
			return fmt.Errorf("无法确定重复序列的目标 schema")
		}
		schema = current.String
	}
	name := dialect.SequenceName(d, seq)
	var kind string
	var reusable bool
	if err := db.QueryRowContext(ctx, pgSequenceReuseSQL, schema, name, seq.TableName, seq.ColumnName).Scan(&kind, &reusable); err != nil {
		return fmt.Errorf("核验同名序列 %s.%s: %w", schema, name, err)
	}
	if kind != "S" {
		return fmt.Errorf("同名对象 %s.%s 不是序列，拒绝复用", schema, name)
	}
	if !reusable {
		return fmt.Errorf("序列 %s.%s 已归属其他表列或被其他表列引用，拒绝复用", schema, name)
	}
	return nil
}
