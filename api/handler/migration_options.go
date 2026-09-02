package handler

// resolveChangeOwner preserves explicit choices, including false. Only PostgreSQL
// compatible targets default to changing the owner to the schema's namesake role.
func resolveChangeOwner(targetType string, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	switch targetType {
	case "postgres", "gaussdb", "seabox", "highgo", "vastbase", "gbase", "kingbase":
		return true
	default:
		return false
	}
}
