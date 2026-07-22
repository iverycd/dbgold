package main

import (
	"context"
	"errors"
	"testing"
)

func TestReadRetentionPrefersSeconds(t *testing.T) {
	got, err := readRetentionWithFallback(context.Background(), func(_ context.Context, name string) (string, error) {
		if name == "binlog_expire_logs_seconds" {
			return "604800", nil
		}
		return "7", nil
	})
	if err != nil || got != "604800s" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestMySQLVariableEnabled(t *testing.T) {
	for _, value := range []string{"ON", "on", "1", "TRUE", " yes "} {
		if !mysqlVariableEnabled(value) {
			t.Fatalf("expected %q to be enabled", value)
		}
	}
	for _, value := range []string{"OFF", "0", "FALSE", "", "2"} {
		if mysqlVariableEnabled(value) {
			t.Fatalf("expected %q to be disabled", value)
		}
	}
}

func TestReadRetentionFallsBackToMySQL57Days(t *testing.T) {
	got, err := readRetentionWithFallback(context.Background(), func(_ context.Context, name string) (string, error) {
		if name == "binlog_expire_logs_seconds" {
			return "", errors.New("Error 1193: Unknown system variable")
		}
		return "7", nil
	})
	if err != nil || got != "604800s（expire_logs_days=7）" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestReadRetentionAllowsDisabledExpiry(t *testing.T) {
	got, err := readRetentionWithFallback(context.Background(), func(_ context.Context, name string) (string, error) {
		if name == "binlog_expire_logs_seconds" {
			return "0", nil
		}
		return "0", nil
	})
	if err != nil || got != "不自动清理（expire_logs_days=0）" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}
