// Package consumer applies the worker's task updates (DESIGN.md §6): it reads
// controlplane.task-updates and hands each update to store.ApplyUpdate, which
// makes it idempotent and order-tolerant. This package decides what happens to
// each message: ack, dead-letter, retry or requeue.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/Ankitwasnik/tenant-service/internal/backoff"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// Store is the consumer's view of persistence. *store.Store implements it.
type Store interface {
	ApplyUpdate(ctx context.Context, u messaging.TaskUpdate) (store.UpdateResult, error)
}

// Config tunes the consumer. Zero values take the defaults (DESIGN.md §6).
type Config struct {
	Prefetch   int           // unacked messages the broker hands us at once; default 16
	Handlers   int           // goroutines applying updates; default 4
	MinBackoff time.Duration // first wait before retrying a transient error; default 100ms
	MaxBackoff time.Duration // cap on that wait; default 30s
}

func (c Config) withDefaults() Config {
	if c.Prefetch <= 0 {
		c.Prefetch = 16
	}
	if c.Handlers <= 0 {
		c.Handlers = 4
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = 100 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 30 * time.Second
	}
	return c
}

type Consumer struct {
	store       Store
	logger      *slog.Logger
	cfg         Config
	isTransient func(error) bool
}

func New(s Store, logger *slog.Logger, cfg Config) *Consumer {
	return &Consumer{store: s, logger: logger, cfg: cfg.withDefaults(), isTransient: store.IsTransient}
}

// Handle applies one delivery and settles it:
//
//   - applied, duplicate or stale: ack. A duplicate and a stale update change
//     nothing, and neither is a failure.
//   - malformed, or a permanent error (unknown task, broken invariant, a bug),
//     or a panic: nack without requeue, so it goes to the DLQ, and the consumer
//     carries on with the next message.
//   - a transient error (the database is unreachable, a deadlock): retry here,
//     with backoff, holding the message. Nothing else can make progress while
//     the database is down, and retrying in-process rather than by requeueing
//     is what bounds it: a requeue isn't counted by the delivery limit.
//   - shutdown during a retry: nack with requeue, which is free.
//
// The returned error is only for a failed ack or nack: the channel is gone,
// and the caller stops.
func (c *Consumer) Handle(ctx context.Context, d amqp.Delivery) (err error) {
	log := c.logger.With("message_id", d.MessageId, "redelivered", d.Redelivered)

	defer func() {
		if rec := recover(); rec != nil {
			log.Error("panic applying update; dead-lettering it",
				"panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			err = d.Nack(false, false)
		}
	}()

	u, err := messaging.DecodeTaskUpdate(d.Body)
	if err != nil {
		log.Error("malformed update; dead-lettering it", "error", err)
		return d.Nack(false, false)
	}
	log = log.With("update_id", u.UpdateID, "task_id", u.TaskID, "status", u.Status)

	retry := backoff.New(c.cfg.MinBackoff, c.cfg.MaxBackoff)
	for attempt := 1; ; attempt++ {
		result, err := c.store.ApplyUpdate(ctx, u)
		switch {
		case err == nil:
			log.Info("update processed", "result", result)
			return d.Ack(false)

		case ctx.Err() != nil:
			log.Info("shutting down; requeueing update")
			return d.Nack(false, true)

		case c.isTransient(err):
			wait := retry.Next()
			log.Warn("transient error applying update; retrying", "error", err, "attempt", attempt, "backoff", wait)
			if backoff.Sleep(ctx, wait) != nil {
				log.Info("shutting down; requeueing update")
				return d.Nack(false, true)
			}

		default:
			level := slog.LevelWarn
			if errors.Is(err, store.ErrInvariant) {
				level = slog.LevelError // a broken guard is a bug, and must be loud
			}
			log.Log(ctx, level, "update can't be applied; dead-lettering it", "error", err)
			return d.Nack(false, false)
		}
	}
}

// Run consumes controlplane.task-updates until ctx is cancelled (then it
// returns nil) or the channel goes away (then it returns an error, and the
// process exits to be restarted).
func (c *Consumer) Run(ctx context.Context, conn *amqp.Connection) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open consumer channel: %w", err)
	}
	defer ch.Close()
	if err := messaging.DeclareTopology(ch); err != nil {
		return err
	}
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	deliveries, err := ch.Consume(messaging.ControlPlaneUpdateQueue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", messaging.ControlPlaneUpdateQueue, err)
	}
	c.logger.Info("update consumer started", "prefetch", c.cfg.Prefetch, "handlers", c.cfg.Handlers)

	// Updates for one task can be handled by two goroutines at once; the task
	// row lock in store.ApplyUpdate makes them take turns (DESIGN.md §5).
	g, gctx := errgroup.WithContext(ctx)
	for range c.cfg.Handlers {
		g.Go(func() error {
			for {
				select {
				case <-gctx.Done():
					return nil
				case d, ok := <-deliveries:
					if !ok {
						if gctx.Err() != nil {
							return nil
						}
						return errors.New("delivery channel closed: the broker connection or channel is gone")
					}
					if err := c.Handle(gctx, d); err != nil {
						return fmt.Errorf("settle delivery: %w", err)
					}
				}
			}
		})
	}
	err = g.Wait()
	c.logger.Info("update consumer stopped")
	return err
}
