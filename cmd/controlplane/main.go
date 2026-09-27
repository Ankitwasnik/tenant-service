// Command controlplane runs the tenant provisioning control plane: the HTTP
// API, the outbox relay and (from PLAN.md 6.2) the task-update consumer
// (DESIGN.md §2).
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

	conn, err := messaging.Dial(cfg.AMQPURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Until the reconnect helper (PLAN.md 7.1), a lost broker connection stops
	// the process, and compose's restart policy brings it back. Unacked
	// messages are redelivered and unpublished events wait in the outbox, so
	// nothing is lost.
	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))

	publisher, err := outbox.NewAMQPPublisher(conn, outbox.DefaultConfirmTimeout)
	if err != nil {
		return err
	}
	logger.Info("broker ready")
	relay := outbox.NewRelay(repo, publisher, logger, outbox.Config{})

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
	g.Go(func() error { return relay.Run(gctx) })
	g.Go(func() error {
		select {
		case <-gctx.Done():
			return nil
		case amqpErr, ok := <-connClosed:
			if !ok || amqpErr == nil { // closed without an error: a clean close
				return errors.New("broker connection closed")
			}
			return fmt.Errorf("broker connection lost: %w", amqpErr)
		}
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
