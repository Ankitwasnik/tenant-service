// Package outbox is the relay that turns committed outbox rows into broker
// messages (DESIGN.md §6). The API never publishes; this is the only path
// from the database to RabbitMQ, and it only ever sees committed rows.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/Ankitwasnik/tenant-service/internal/backoff"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// Store is the relay's view of persistence. *store.Store implements it.
type Store interface {
	RelayOutbox(ctx context.Context, limit int, publish store.PublishFunc) (claimed, marked int, err error)
}

// Publisher sends a batch to the broker and reports which messages it
// confirmed. Anything not in confirmed (nacked, returned as unroutable, or
// unconfirmed within the timeout) is treated as not published.
type Publisher interface {
	Publish(ctx context.Context, msgs []store.OutboxMessage) (confirmed []int64, err error)
}

// Config tunes the relay. Zero values take the defaults (DESIGN.md §6).
type Config struct {
	PollInterval time.Duration // idle wait between polls; default 200ms
	BatchSize    int           // rows claimed per step; default 100
	MinBackoff   time.Duration // first wait after a failure; default 100ms
	MaxBackoff   time.Duration // cap on the exponential backoff; default 10s
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 200 * time.Millisecond
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = 100 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 10 * time.Second
	}
	return c
}

type Relay struct {
	store     Store
	publisher Publisher
	logger    *slog.Logger
	cfg       Config
}

func NewRelay(s Store, p Publisher, logger *slog.Logger, cfg Config) *Relay {
	return &Relay{store: s, publisher: p, logger: logger, cfg: cfg.withDefaults()}
}

// Run relays until ctx is cancelled, then returns nil. A failed step (broker
// down, database down, a message not confirmed) is logged and retried with
// jittered exponential backoff; the API keeps accepting writes meanwhile, and
// the events wait in the outbox until the relay catches up.
func (r *Relay) Run(ctx context.Context) error {
	r.logger.Info("outbox relay started")
	retry := backoff.New(r.cfg.MinBackoff, r.cfg.MaxBackoff)
	for {
		claimed, marked, err := r.Step(ctx)
		if ctx.Err() != nil {
			r.logger.Info("outbox relay stopped")
			return nil
		}

		wait := r.cfg.PollInterval
		switch {
		case err != nil:
			wait = retry.Next()
			r.logger.Warn("outbox relay step failed; backing off",
				"error", err, "claimed", claimed, "published", marked, "backoff", wait)
		case claimed == r.cfg.BatchSize:
			// A full batch: there may be more waiting, so go again right away.
			retry.Reset()
			wait = 0
		default:
			retry.Reset()
		}

		if wait > 0 {
			if err := backoff.Sleep(ctx, wait); err != nil {
				r.logger.Info("outbox relay stopped")
				return nil
			}
		}
	}
}

// Step runs one claim → publish → mark → commit cycle. Run calls it in a
// loop; tests call it directly.
func (r *Relay) Step(ctx context.Context) (claimed, marked int, err error) {
	claimed, marked, err = r.store.RelayOutbox(ctx, r.cfg.BatchSize, r.publisher.Publish)
	if marked > 0 {
		r.logger.Info("outbox events published", "count", marked)
	}
	return claimed, marked, err
}
