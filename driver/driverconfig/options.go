package driverconfig

// ConnectOptions keeps credentials separate from protocol URLs. Native Go
// drivers use DSN; JDBC-backed drivers also consume Username and Password.
type ConnectOptions struct {
	DSN      string
	Username string
	Password string
}
