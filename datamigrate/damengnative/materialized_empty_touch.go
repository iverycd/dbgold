package damengnative

// MaterializedEmptyTouch freezes the temporary attributes used to advance an
// empty base table's native DML epoch. The INSERT and DELETE run in one
// transaction; original defaults, nullability, constraints and identity are
// restored before the group can succeed. No expression comes from the SQL
// repair editor.
type MaterializedEmptyTouch struct {
	Table           string                        `json:"table"`
	DefinitionSQL   string                        `json:"definition_sql"`
	Columns         []MaterializedEmptyColumn     `json:"columns"`
	Constraints     []MaterializedEmptyConstraint `json:"constraints,omitempty"`
	IdentityRestore []string                      `json:"identity_restore,omitempty"`
}

type MaterializedEmptyColumn struct {
	Name          string `json:"name"`
	NotNull       bool   `json:"not_null"`
	DefaultSQL    string `json:"default_sql,omitempty"`
	DefaultOnNull bool   `json:"default_on_null,omitempty"`
	ValueSQL      string `json:"value_sql,omitempty"` // frozen non-NULL scalar seed for an immutable clustered key
}

type MaterializedEmptyConstraint struct {
	DefinitionSQL string   `json:"definition_sql,omitempty"`
	Name          string   `json:"name"`
	Table         string   `json:"table"`
	Kind          string   `json:"kind"`
	Columns       []string `json:"columns"`
	Generated     bool     `json:"generated,omitempty"`
	Enabled       bool     `json:"enabled"`
	NoValidate    bool     `json:"no_validate,omitempty"`
	Clustered     bool     `json:"clustered,omitempty"`
}
