package driver

type Driver interface {
	Connect(dsn string) error
	Ping() error
	Close() error
}
