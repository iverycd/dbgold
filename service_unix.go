//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func runManaged(run func(context.Context) error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return run(ctx)
}
