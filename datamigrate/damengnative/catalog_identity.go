package damengnative

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// NativeColumnIdentity reads only the named column declaration. On DM8 a
// partition parent's SYSCOLUMNS.INFO2 can be zero while its native declaration
// still has a real IDENTITY attribute. Literals, defaults, other columns and
// constraint expressions are not identity metadata.
func NativeColumnIdentity(ddl, column string) (bool, error) {
	ts, err := nativeTokens(ddl)
	if err != nil {
		return false, err
	}
	depth, start := 0, -1
	inspect := func(end int) bool {
		if start < 0 || start >= end {
			return false
		}
		first := ts[start]
		if first.kind != 'q' && first.kind != 'i' {
			return false
		}
		name := first.value
		if first.kind == 'i' {
			name = strings.ToUpper(name)
		}
		if name != column {
			return false
		}
		local := 0
		for i := start + 1; i < end; i++ {
			t := ts[i]
			if t.kind == 'p' {
				if t.value == "(" {
					local++
				}
				if t.value == ")" {
					local--
				}
			}
			if local != 0 || t.kind != 'i' {
				continue
			}
			switch strings.ToUpper(t.value) {
			case "DEFAULT", "GENERATED", "CONSTRAINT", "CHECK", "REFERENCES":
				return false
			case "IDENTITY":
				return i+1 < end && ts[i+1].value == "("
			}
		}
		return false
	}
	for i, t := range ts {
		if t.kind != 'p' {
			continue
		}
		switch t.value {
		case "(":
			depth++
			if depth == 1 && start < 0 {
				start = i + 1
			}
		case ",":
			if depth == 1 {
				if inspect(i) {
					return true, nil
				}
				start = i + 1
			}
		case ")":
			depth--
			if depth == 0 && start >= 0 {
				return inspect(i), nil
			}
		}
	}
	return false, nil
}

func declaredNativeIdentity(o Object, column string) bool {
	for _, u := range o.Units {
		if u.Phase != "before_data" {
			continue
		}
		ts, e := nativeTokens(u.SQL)
		if e != nil || len(ts) < 2 || !strings.EqualFold(ts[0].value, "CREATE") {
			continue
		}
		for i := 1; i < len(ts) && i < 5; i++ {
			if ts[i].kind == 'i' && strings.EqualFold(ts[i].value, "TABLE") {
				found, _ := NativeColumnIdentity(u.SQL, column)
				if found {
					return true
				}
				break
			}
		}
	}
	return false
}

// The public IDENT_CURRENT value of a horizontal partition root can lag its
// allocations. SYSOBJECTS.INFO6 bytes 37-40 identify the active preallocated
// sequence, bytes 41-42 its kind and 43-44 its column. These byte positions are
// documented in DM's administrator manual, appendix 1. Old DROP/ADD sequences
// may retain INFO2=table ID, so that relation alone never selects the active one.
func (r *catalogReader) partitionIdentityCurrent(table, column string, current, step int64) (int64, error) {
	var root int64
	for id, object := range r.raw {
		if object.sub == "UTAB" && object.name == table && object.pid <= 0 {
			root = id
			break
		}
	}
	partitioned := false
	for _, object := range r.raw {
		partitioned = partitioned || root > 0 && object.sub == "UTAB" && object.pid == root
	}
	if !partitioned {
		return current, nil
	}
	var info []byte
	var colID int
	if err := r.db.QueryRowContext(r.ctx, `SELECT t.INFO6,c.COLID FROM SYS.SYSOBJECTS t JOIN SYS.SYSCOLUMNS c ON c.ID=t.ID WHERE t.ID=? AND c.NAME=?`, root, column).Scan(&info, &colID); err != nil {
		return 0, err
	}
	active, err := partitionIdentitySequence(info, colID)
	if err != nil {
		return 0, err
	}
	sequence, known := r.raw[active]
	if !known || sequence.sub != "SEQ" || sequence.info2 != root {
		return 0, fmt.Errorf("分区自增列活动序列与冻结原生父链不同")
	}
	var lastText string
	if err = r.db.QueryRowContext(r.ctx, `SELECT CAST(SF_SEQUENCE_GET_LAST_NUM(?,?) AS VARCHAR(80)) FROM DUAL`, r.schema, sequence.name).Scan(&lastText); err != nil {
		return 0, err
	}
	last, err := strconv.ParseInt(lastText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("分区自增列活动序列状态不可精确读取: %w", err)
	}
	// Explicit identity inserts can exceed even the active allocator state.
	// Retain a bound beyond every existing value as well as allocated numbers.
	aggregate := "MAX"
	if step < 0 {
		aggregate = "MIN"
	}
	var edge sql.NullString
	if err = r.db.QueryRowContext(r.ctx, `SELECT CAST(`+aggregate+`(`+Quote(column)+`) AS VARCHAR(80)) FROM `+Qualified(r.schema, table)).Scan(&edge); err != nil {
		return 0, err
	}
	if edge.Valid {
		value, e := strconv.ParseInt(edge.String, 10, 64)
		if e != nil {
			return 0, fmt.Errorf("分区自增列已有值边界不可精确读取: %w", e)
		}
		if step > 0 && value > current || step < 0 && value < current {
			current = value
		}
	}
	return partitionIdentityPredecessor(last, current, step)
}

// LAST_NUM is the next preallocated boundary. Advance along that exact
// progression only when it could collide with the public/current data bound.
// Return its predecessor for identityRestore's common +step path.
func partitionIdentityPredecessor(next, bound, step int64) (int64, error) {
	if step == 0 {
		return 0, fmt.Errorf("分区自增步长不能为零")
	}
	n, b, s := big.NewInt(next), big.NewInt(bound), big.NewInt(step)
	if step > 0 && n.Cmp(b) <= 0 || step < 0 && n.Cmp(b) >= 0 {
		distance := new(big.Int).Sub(b, n)
		if step < 0 {
			distance.Neg(distance)
		}
		stride := new(big.Int).Abs(s)
		number := new(big.Int).Quo(distance, stride)
		number.Add(number, big.NewInt(1))
		n.Add(n, new(big.Int).Mul(number, s))
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("分区自增列下一值溢出int64")
	}
	n.Sub(n, s)
	if !n.IsInt64() {
		return 0, fmt.Errorf("分区自增列续号前驱溢出int64")
	}
	return n.Int64(), nil
}

func partitionIdentitySequence(info []byte, colID int) (int64, error) {
	if len(info) < 44 {
		return 0, fmt.Errorf("分区自增列原生序列元数据不完整")
	}
	active := int64(binary.LittleEndian.Uint32(info[36:40]))
	if active <= 0 || binary.LittleEndian.Uint16(info[40:42]) != 1 || int(binary.LittleEndian.Uint16(info[42:44])) != colID {
		return 0, fmt.Errorf("分区自增列原生类型、列或活动序列不一致")
	}
	return active, nil
}
