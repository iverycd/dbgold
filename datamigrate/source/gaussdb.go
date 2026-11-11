package source

// GaussDBDistribution is target metadata, never populated by source Readers.
// OID binds repair plans to a particular table, including drop/recreate drift.
type GaussDBDistribution struct {
	Schema      string            `json:"schema"`
	Table       string            `json:"table"`
	OID         uint32            `json:"oid"`
	Mode        string            `json:"mode"`
	Columns     []string          `json:"columns"`
	Types       map[string]string `json:"types"`
	Kind        string            `json:"kind"`
	Persistence string            `json:"persistence"`
	Partition   string            `json:"partition"`
	Options     []string          `json:"options"`
}

// GaussDBIndexPlan is separate from Locality (partition index locality).
type GaussDBIndexPlan struct {
	Base   GaussDBDistribution `json:"base"`
	Global bool                `json:"global"`
	// Empty for BY GLOBAL INDEX: the kernel selects a subset of the constraint
	// keys. Explicit CREATE GLOBAL INDEX freezes the chosen distribution here.
	Columns   []string `json:"columns,omitempty"`
	Version   string   `json:"version,omitempty"`
	GTMFree   string   `json:"gtm_free,omitempty"`
	GTMOption string   `json:"gtm_option,omitempty"`
}
