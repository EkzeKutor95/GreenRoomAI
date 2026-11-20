package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"greenroom-ai/internal/supervisor"
)

func runSupervised(logger *slog.Logger, envFile string, interval time.Duration) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate application executable: %w", err)
	}
	runner, err := supervisor.New(supervisor.Config{
		Executable:      executable,
		Args:            []string{"--worker", "--env", envFile},
		RestartInterval: interval,
		ShutdownTimeout: 30 * time.Second,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure application supervisor: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runner.Run(ctx)
}

func contextFromControlPipe(parent context.Context, input io.Reader) context.Context {
	ctx, cancel := context.WithCancelCause(parent)
	// This reader belongs to a single child process. Its lifetime ends with that
	// process; EOF requests the same graceful shutdown as an operating-system signal.
	go func() {
		if _, err := io.Copy(io.Discard, input); err != nil {
			cancel(fmt.Errorf("read supervisor control pipe: %w", err))
			return
		}
		cancel(nil)
	}()
	return ctx
}
