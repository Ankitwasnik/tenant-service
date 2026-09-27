// Command controlplane runs the tenant provisioning control plane: the HTTP
// API, the outbox relay and the task-update consumer, as goroutines in one
// errgroup (DESIGN.md §2).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/Ankitwasnik/tenant-service/internal/api"
	"github.com/Ankitwasnik/tenant-service/internal/consumer"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/outbox"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// HTTP server timeouts. Every request is a small JSON exchange, so these are
// generous; they exist so a slow or stuck client can't hold a connection forever.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	// The URLs carry credentials, so only the listen address is logged.
	logger.Info("starting", "http_addr", cfg.HTTPAddr)

	// SIGTERM (docker stop) and SIGINT (Ctrl-C) cancel ctx, which stops everything.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL) // also applies pending migrations
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("database ready")
	repo := store.New(pool)

	updates := consumer.New(repo, logger, consumer.Config{})

	// Everything that needs the broker runs in one session per connection:
	// messaging.Run redials with backoff when the connection is lost, and
	// starts a fresh relay and consumer on the new one. The HTTP API is
	// outside it, so it keeps accepting writes while the broker is down; their
	// events wait in the outbox (DESIGN.md §6).
	brokerSession := func(ctx context.Context, conn *amqp.Connection) error {
		publisher, err := outbox.NewAMQPPublisher(conn, outbox.DefaultConfirmTimeout)
		if err != nil {
			return err
		}
		defer publisher.Close()
		relay := outbox.NewRelay(repo, publisher, logger, outbox.Config{})

		sg, sctx := errgroup.WithContext(ctx)
		sg.Go(func() error { return relay.Run(sctx) })
		sg.Go(func() error { return updates.Run(sctx, conn) })
		return sg.Wait()
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(api.Deps{Logger: logger, DB: pool, Store: repo}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		return messaging.Run(gctx, messaging.ReconnectConfig{URL: cfg.AMQPURL, Logger: logger}, brokerSession)
	})
	g.Go(func() error {
		<-gctx.Done() // a signal, or another goroutine failed
		logger.Info("shutting down")
		// A fresh context: gctx is already cancelled, and in-flight requests
		// need time to finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}
