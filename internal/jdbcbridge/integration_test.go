package jdbcbridge

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOscarIntegrationConnectionAndSchemas(t *testing.T) {
	url := os.Getenv("OSCAR_TEST_URL")
	username := os.Getenv("OSCAR_TEST_USER")
	password := os.Getenv("OSCAR_TEST_PASSWORD")
	schema := os.Getenv("OSCAR_TEST_SCHEMA")
	if url == "" || username == "" || password == "" || schema == "" {
		t.Skip("OSCAR_TEST_URL/USER/PASSWORD/SCHEMA are not all set")
	}
	manager := NewManager(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.OpenSession(ctx, SessionOptions{
		URL: url, Username: username, Password: password, Schema: schema,
		MaxOpenConns: 2, MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	defer manager.CloseSession(context.Background(), session)
	if err := manager.Ping(ctx, session); err != nil {
		t.Fatal(err)
	}
	exists, err := manager.SchemaExists(ctx, session, schema)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("configured schema %q not visible through DatabaseMetaData", schema)
	}
}
