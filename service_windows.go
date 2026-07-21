//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "dbgold"

func runManaged(run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service: %w", err)
	}
	if !isService {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		return run(ctx)
	}
	handler := &windowsService{run: run}
	if err := svc.Run(windowsServiceName, handler); err != nil {
		return fmt.Errorf("run Windows service: %w", err)
	}
	return handler.err
}

type windowsService struct {
	run func(context.Context) error
	err error
}

func (s *windowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				s.err = <-done
				return false, serviceExitCode(s.err)
			}
		case err := <-done:
			s.err = err
			return false, serviceExitCode(err)
		}
	}
}

func serviceExitCode(err error) uint32 {
	if err == nil {
		return 0
	}
	return 1
}
