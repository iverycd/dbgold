package handler

// resolveChangeOwner preserves explicit choices, including false. Oscar alone
// defaults to keeping the connected role as owner of newly created objects.
func resolveChangeOwner(targetType string, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	return targetType != "oscar"
}
