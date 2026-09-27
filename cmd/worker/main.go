// Command worker is the provisioning simulator (DESIGN.md §8): it consumes
// task events and reports in_progress, then done or failed. Timing and
// outcome are set at startup by flags or WORKER_* env vars, so failure and
// delay scenarios are a restart away (`make worker ARGS=...`).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	amqp "github.com/rabbitmq/amqp091-go"

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

	// On a lost connection, messaging.Run redials with backoff and starts the
	// worker again on the new one. Unacked tasks are redelivered by RabbitMQ.
	return messaging.Run(ctx, messaging.ReconnectConfig{URL: amqpURL, Logger: logger},
		func(ctx context.Context, conn *amqp.Connection) error {
			return worker.Run(ctx, conn, cfg, logger)
		})
}
