// Command worker is the provisioning simulator (DESIGN.md §8): it consumes
// task events and reports in_progress, then done or failed. Timing and
// outcome are set at startup by flags or WORKER_* env vars, so failure and
// delay scenarios are a restart away (`make worker ARGS=...`).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, amqpURL, err := parseConfig(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	conn, err := messaging.Dial(amqpURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	// As in the control plane: until the reconnect helper (PLAN.md 7.1), a
	// lost connection stops the process and compose restarts it. Unacked
	// tasks are redelivered.
	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return worker.Run(gctx, conn, cfg, logger) })
	g.Go(func() error {
		select {
		case <-gctx.Done():
			return nil
		case amqpErr, ok := <-connClosed:
			if !ok || amqpErr == nil {
				return errors.New("broker connection closed")
			}
			return fmt.Errorf("broker connection lost: %w", amqpErr)
		}
	})
	return g.Wait()
}
