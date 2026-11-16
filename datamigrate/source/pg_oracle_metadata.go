package source

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"dbgold/datamigrate/failure"
)

// loadPGOracleSequences deliberately does not use GetSequences: that legacy
// interface models an AUTO_INCREMENT column and rejects independent/shared
// generators. The Oracle snapshot keeps the actual named source objects.
func (r *PostgresReader) loadPGOracleSequences(ctx context.Context, snap *PGSnapshot, selected map[string]bool) error {
	rows, err := r.db.QueryContext(ctx, `SELECT c.oid,n.nspname,c.relname FROM `+r.qcat("pg_class")+` c JOIN `+r.qcat("pg_namespace")+` n ON n.oid=c.relnamespace WHERE c.relkind='S' AND (n.nspname=$1 OR EXISTS (SELECT 1 FROM `+r.qcat("pg_depend")+` d JOIN `+r.qcat("pg_attrdef")+` a ON a.oid=d.objid JOIN `+r.qcat("pg_class")+` t ON t.oid=a.adrelid JOIN `+r.qcat("pg_namespace")+` tn ON tn.oid=t.relnamespace WHERE d.refobjid=c.oid AND tn.nspname=$1)) ORDER BY n.nspname,c.relname`, r.schema)
	if err != nil {
		return err
	}
	var sequences []PGSequence
	for rows.Next() {
		var seq PGSequence
		if err = rows.Scan(&seq.OID, &seq.Schema, &seq.Name); err != nil {
			break
		}
		sequences = append(sequences, seq)
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
	for _, seq := range sequences {
		seq.Bindings, err = r.pgOracleSequenceBindings(ctx, seq.OID)
		if err != nil {
			if failure.IsTaskError(err) {
				return err
			}
			seq.Unsupported = fmt.Sprintf("读取序列 %s.%s 的依赖: %v", seq.Schema, seq.Name, err)
			r.appendPGOracleSequence(snap, seq)
			continue
		}
		relevant := seq.Schema == r.schema && (selected == nil || len(seq.Bindings) == 0)
		for _, binding := range seq.Bindings {
			if binding.Schema == r.schema && (selected == nil || selected[binding.TableName]) {
				relevant = true
			}
		}
		if !relevant {
			continue // Do not require state-read privileges for excluded owners.
		}
		seq.Shared = len(seq.Bindings) > 1
		if len(seq.Bindings) == 1 {
			seq.Identity = seq.Bindings[0].Identity
		}
		qn := QuotePGIdent(seq.Schema) + "." + QuotePGIdent(seq.Name)
		err = r.db.QueryRowContext(ctx, `SELECT last_value,is_called FROM `+qn).Scan(&seq.LastValue, &seq.IsCalled)
		if err == nil {
			if r.profile.Fields["pg_sequence.seqincrement"] {
				err = r.db.QueryRowContext(ctx, `SELECT seqincrement,seqmin,seqmax,seqcycle,seqcache FROM `+r.qcat("pg_sequence")+` WHERE seqrelid=$1`, seq.OID).Scan(&seq.Increment, &seq.Min, &seq.Max, &seq.Cycle, &seq.Cache)
			} else {
				err = r.db.QueryRowContext(ctx, `SELECT increment_by,min_value,max_value,is_cycled,cache_value FROM `+qn).Scan(&seq.Increment, &seq.Min, &seq.Max, &seq.Cycle, &seq.Cache)
			}
		}
		if err == nil {
			err = pgSequenceNextValue(&seq)
		}
		if err != nil {
			if failure.IsTaskError(err) {
				return err
			}
			seq.Unsupported = fmt.Sprintf("读取序列 %s.%s: %v", seq.Schema, seq.Name, err)
		}
		r.appendPGOracleSequence(snap, seq)
	}
	return nil
}

func (r *PostgresReader) pgOracleSequenceBindings(ctx context.Context, oid int64) ([]PGSequenceBinding, error) {
	identity := r.field("pg_attribute", "attidentity", "a", "''")
	class := `'` + r.qcat("pg_class") + `'::regclass`
	owned := `EXISTS(SELECT 1 FROM ` + r.qcat("pg_depend") + ` own WHERE own.objid=$1 AND own.classid=` + class + ` AND own.refclassid=` + class + ` AND own.refobjid=t.oid AND own.refobjsubid=a.attnum AND own.deptype IN ('a','i'))`
	query := `SELECT n.nspname,t.relname,a.attname,COALESCE(pg_get_expr(ad.adbin,ad.adrelid),''),` + identity + `,` + owned + ` FROM ` + r.qcat("pg_attribute") + ` a JOIN ` + r.qcat("pg_class") + ` t ON t.oid=a.attrelid JOIN ` + r.qcat("pg_namespace") + ` n ON n.oid=t.relnamespace LEFT JOIN ` + r.qcat("pg_attrdef") + ` ad ON ad.adrelid=t.oid AND ad.adnum=a.attnum WHERE a.attnum>0 AND NOT a.attisdropped AND (` + owned + ` OR EXISTS(SELECT 1 FROM ` + r.qcat("pg_depend") + ` d WHERE d.refobjid=$1 AND d.refclassid=` + class + ` AND d.objid=ad.oid AND d.classid='` + r.qcat("pg_attrdef") + `'::regclass)) ORDER BY n.nspname,t.relname,a.attnum`
	rows, err := r.db.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []PGSequenceBinding
	for rows.Next() {
		var b PGSequenceBinding
		if err := rows.Scan(&b.Schema, &b.TableName, &b.ColumnName, &b.Default, &b.Identity, &b.Owned); err != nil {
			return nil, err
		}
		b.Identity = strings.Trim(b.Identity, "\x00")
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

func pgSequenceNextValue(seq *PGSequence) error {
	if seq.Increment == 0 || seq.Min >= seq.Max || seq.Cache < 1 || seq.LastValue < seq.Min || seq.LastValue > seq.Max {
		return fmt.Errorf("序列状态或增量/边界/缓存元数据无效")
	}
	seq.StartValue = seq.LastValue
	if !seq.IsCalled {
		return nil
	}
	next := new(big.Int).Add(big.NewInt(seq.LastValue), big.NewInt(seq.Increment))
	if next.Cmp(big.NewInt(seq.Min)) < 0 || next.Cmp(big.NewInt(seq.Max)) > 0 {
		if !seq.Cycle {
			seq.Exhausted = true
			return nil
		}
		if seq.Increment > 0 {
			seq.StartValue = seq.Min
		} else {
			seq.StartValue = seq.Max
		}
		return nil
	}
	seq.StartValue = next.Int64()
	return nil
}

func (r *PostgresReader) appendPGOracleSequence(snap *PGSnapshot, seq PGSequence) {
	snap.Sequences = append(snap.Sequences, seq)
	if seq.Unsupported != "" {
		snap.Objects = append(snap.Objects, PGObject{Kind: "sequence", Name: seq.Name, Reason: seq.Unsupported})
	}
	for _, binding := range seq.Bindings {
		if binding.Schema != r.schema {
			continue
		}
		for i := range snap.Tables {
			t := &snap.Tables[i]
			if t.Info.TableName != binding.TableName {
				continue
			}
			copy := seq
			copy.TableName, copy.ColumnName, copy.Identity = binding.TableName, binding.ColumnName, binding.Identity
			t.Sequences = append(t.Sequences, copy)
			if seq.Unsupported != "" {
				t.Unsupported = append(t.Unsupported, PGObject{Kind: "sequence", Table: binding.TableName, Name: seq.Name, Reason: seq.Unsupported})
			}
		}
	}
}
