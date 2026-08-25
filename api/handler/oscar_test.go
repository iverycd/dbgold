package handler

import (
	"strings"
	"testing"

	"dbgold/store"
	"github.com/gin-gonic/gin/binding"
)

func TestOscarValidationDSNAndMigrationPairs(t *testing.T) {
	connection := &connectionRequest{
		Name: "oscar", DBType: "oscar", Host: "127.0.0.1", Port: 2003,
		Database: "OSRDB", Username: "user", Password: "secret",
	}
	if err := binding.Validator.ValidateStruct(connection); err != nil {
		t.Fatalf("Oscar connection request rejected: %v", err)
	}
	dsn := buildDSN(&store.Connection{DBType: "oscar", Host: "db", Port: 2003, Database: "OSRDB", Username: "user", Password: "secret"})
	if dsn != "jdbc:oscar://db:2003/OSRDB" {
		t.Fatalf("Oscar URL = %q", dsn)
	}
	if strings.Contains(dsn, "user") || strings.Contains(dsn, "secret") {
		t.Fatalf("Oscar URL leaks credentials: %q", dsn)
	}

	targetOnly := &submitTicketRequest{
		CaptchaID: "captcha", CaptchaCode: "code", SrcDBType: "mysql", DstDBType: "oscar",
	}
	if err := binding.Validator.ValidateStruct(targetOnly); err != nil {
		t.Fatalf("Oscar ticket target rejected: %v", err)
	}
	source := &submitTicketRequest{
		CaptchaID: "captcha", CaptchaCode: "code", SrcDBType: "oscar", DstDBType: "postgres",
	}
	if err := binding.Validator.ValidateStruct(source); err == nil {
		t.Fatal("Oscar must not be accepted as a ticket source")
	}

	for _, src := range []string{"mysql", "sqlserver", "oracle", "dameng"} {
		if !isSupportedPair(src, "oscar") {
			t.Errorf("missing migration pair %s -> oscar", src)
		}
	}
	if isSupportedPair("oscar", "postgres") {
		t.Fatal("Oscar source must not be supported")
	}
	if normalizeDBType("ShenTong") != "oscar" || normalizeDBType("OSCAR") != "oscar" {
		t.Fatal("Oscar batch aliases are not registered")
	}
}
