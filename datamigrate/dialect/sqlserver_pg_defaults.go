package dialect

func sqlServerPGDefault(raw string) (string, bool, error) {
	return pgDefaultLiteral(raw, "SQL Server")
}
