package handler

import (
	"net/http/httptest"
	"testing"

	"dbgold/config"
	"dbgold/store"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

func TestVastbaseRequestValidationAndMigrationPairs(t *testing.T) {
	connection := &connectionRequest{
		Name: "vastbase", DBType: "vastbase", Host: "127.0.0.1", Port: 5432,
		Database: "postgres", Username: "user", Password: "password",
	}
	if err := binding.Validator.ValidateStruct(connection); err != nil {
		t.Fatalf("vastbase connection request rejected: %v", err)
	}

	submit := &submitTicketRequest{
		CaptchaID: "captcha", CaptchaCode: "code",
		SrcDBType: "vastbase", DstDBType: "vastbase",
	}
	if err := binding.Validator.ValidateStruct(submit); err != nil {
		t.Fatalf("vastbase ticket submission rejected: %v", err)
	}

	update := &updateTicketInfoRequest{SrcDBType: "vastbase", DstDBType: "vastbase"}
	if err := binding.Validator.ValidateStruct(update); err != nil {
		t.Fatalf("vastbase ticket update rejected: %v", err)
	}

	if got := buildDSN(&store.Connection{DBType: "vastbase", Host: "db", Port: 5432, Database: "postgres", Username: "user", Password: "password"}); got != "host=db port=5432 user=user password=password dbname=postgres sslmode=disable" {
		t.Fatalf("vastbase DSN = %q", got)
	}

	for _, pair := range [][2]string{
		{"mysql", "vastbase"},
		{"sqlserver", "vastbase"},
		{"dameng", "vastbase"},
		{"oracle", "vastbase"},
		{"vastbase", "dameng"},
	} {
		if !isSupportedPair(pair[0], pair[1]) {
			t.Errorf("missing migration pair %s -> %s", pair[0], pair[1])
		}
	}
}

func TestVastbaseAllowedInQueryCenter(t *testing.T) {
	store.Init(&config.Config{SQLitePath: ":memory:"})
	connection := &store.Connection{
		OwnerID: 1, Name: "vastbase", DBType: "vastbase", Host: "127.0.0.1", Port: 5432,
		Database: "postgres", Username: "user", Password: "password",
	}
	if err := store.CreateConnection(connection); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("userID", uint(1))
	ctx.Set("role", "user")
	got, ok := ownedQueryConnection(ctx, connection.ID)
	if !ok || got.ID != connection.ID {
		t.Fatal("Vastbase connection should be accepted by the query center")
	}
}
