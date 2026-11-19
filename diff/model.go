// Package diff compares database catalogs and plans additive target DDL.
package diff

import (
	"context"
	"database/sql"

	"dbgold/datamigrate/source"
)

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Catalog struct {
	Product, Namespace, Identity, Mode string
	CaseMode, IdentifierLimit          int
	Tables                             map[string]*Table
	Occupied                           map[string]string
	TransactionalDDL                   bool
}

type Table struct {
	Name, Identity, Comment, Unsupported string
	Engine, Charset, Collation           string
	Columns                              []Column
	Keys, Indexes                        []Key
}

type Column struct {
	source.ColumnInfo
	Collation, Charset, Unsupported string
}

type Key struct {
	Name, Kind, Method, RefNamespace, RefTable, OnUpdate, OnDelete, Definition, Unsupported string
	Columns, RefColumns                                                                     []string
	Descending                                                                              []bool
	PrefixLengths                                                                           []int
}

const (
	Applicable = "applicable"
	Manual     = "manual"
)

type Change struct {
	ID             string   `json:"id"`
	Table          string   `json:"table"`
	Object         string   `json:"object,omitempty"`
	Kind           string   `json:"kind"`
	Status         string   `json:"status"`
	Reason         string   `json:"reason,omitempty"`
	Source         any      `json:"source,omitempty"`
	Expected       any      `json:"expected,omitempty"`
	Target         any      `json:"target,omitempty"`
	SQL            []string `json:"sql,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
	Order          int      `json:"order"`
	ExecutionOrder int      `json:"execution_order,omitempty"`
}

type Options struct {
	Tables                                   []string
	PreserveCase, CharInLength, UseNvarchar2 bool
	// MySQLColumnComment prepares an exact MODIFY using the target's full definition.
	MySQLColumnComment func(table, column, comment string) (string, error)
}

type Pair struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

var pgProducts = []string{"postgres", "gaussdb", "highgo", "vastbase", "gbase", "seabox", "kingbase"}

func SupportedPairs() []Pair {
	pairs := []Pair{{"mysql", "mysql"}, {"mysql", "dameng"}}
	for _, target := range pgProducts {
		pairs = append(pairs, Pair{"mysql", target})
		for _, src := range pgProducts {
			pairs = append(pairs, Pair{src, target})
		}
	}
	return pairs
}

func SupportsPair(src, target string) bool {
	return src == "mysql" && (target == "mysql" || target == "dameng" || source.IsPGFamily(target)) ||
		source.IsPGFamily(src) && source.IsPGFamily(target)
}
