package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/backoff"
)

// Session is the work done on one broker connection: the relay and consumer
// in the control plane, the worker in the worker. It must return once ctx is
// done; ctx is cancelled as soon as the connection is lost.
type Session func(ctx context.Context, conn *amqp.Connection) error

// ReconnectConfig configures Run. Zero values take the defaults.
type ReconnectConfig struct {
	URL    string
	Logger *slog.Logger

	MinBackoff time.Duration // first wait after a failure; default 500ms
	MaxBackoff time.Duration // cap on the wait; default 15s
	// HealthyAfter: a connection that stayed up this long resets the backoff,
	// so a broker that restarts once a day isn't retried at the 15s cap.
	// Default 30s.
	HealthyAfter time.Duration

	// Dial connects; tests replace it. Default: Dial (amqp.Dial).
	Dial func(url string) (*amqp.Connection, error)
}

func (c ReconnectConfig) withDefaults() ReconnectConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = 500 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Second
	}
	if c.HealthyAfter <= 0 {
		c.HealthyAfter = 30 * time.Second
	}
	if c.Dial == nil {
		c.Dial = Dial
	}
	return c
}

// Run keeps a broker connection alive for session until ctx is done, then
// returns nil (DESIGN.md §6):
//
//	dial → declare the topology → session(connCtx, conn)
//	→ connection lost, or session returned → close, back off with jitter → repeat
//
// connCtx is cancelled the moment the connection closes (NotifyClose), so
// everything using it stops together. Messages that were delivered but not
// acked are redelivered by RabbitMQ, and unpublished events stay in the
// outbox, so a reconnect loses nothing. A failed dial or topology declaration
// is retried the same way, so a broker that isn't up yet at startup is waited
// for, not fatal.
func Run(ctx context.Context, cfg ReconnectConfig, session Session) error {
	cfg = cfg.withDefaults()
	retry := backoff.New(cfg.MinBackoff, cfg.MaxBackoff)

	for attempt := 1; ; attempt++ {
		started := time.Now()
		err := runOnce(ctx, cfg, session)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) >= cfg.HealthyAfter {
			retry.Reset()
		}
		wait := retry.Next()
		cfg.Logger.Warn("broker connection lost; reconnecting", "error", err, "attempt", attempt, "backoff", wait)
		if backoff.Sleep(ctx, wait) != nil {
			return nil
		}
	}
}

// runOnce is one connection's life. It returns why it ended.
func runOnce(ctx context.Context, cfg ReconnectConfig, session Session) error {
	conn, err := cfg.Dial(cfg.URL)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := declareOn(conn); err != nil {
		return err
	}
	cfg.Logger.Info("broker connected")

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	lost := make(chan error, 1)
	go func() {
		select {
		case amqpErr, ok := <-closed:
			if ok && amqpErr != nil {
				lost <- fmt.Errorf("connection closed: %w", amqpErr)
			} else {
				lost <- fmt.Errorf("connection closed")
			}
			cancel()
		case <-connCtx.Done():
		}
	}()

	sessionErr := session(connCtx, conn)
	select {
	case err := <-lost:
		return err // the connection went away; that is the reason, not the session's error
	default:
	}
	if sessionErr != nil {
		return fmt.Errorf("session ended: %w", sessionErr)
	}
	return fmt.Errorf("session ended")
}

// declareOn declares the topology on a short-lived channel. Doing it here, on
// every connect, means a queue deleted while we were disconnected is back
// before anything consumes or publishes.
func declareOn(conn *amqp.Connection) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()
	return DeclareTopology(ch)
}
