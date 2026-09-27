// Package worker is the provisioning simulator (DESIGN.md §8). It consumes
// task events, pretends to do the work, and reports progress back over the
// broker: in_progress, then done or failed.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
)

// Concurrency is how many tasks the worker handles at once, and its prefetch.
const Concurrency = 4

// FailureMessage is the error a simulated failure reports.
const FailureMessage = "simulated failure"

// Config is the worker's behavior, set at startup (DESIGN.md §8).
type Config struct {
	MinDelay time.Duration // lower bound of the random delay before each step
	MaxDelay time.Duration // upper bound
	FailRate float64       // probability that the outcome is failed, 0 to 1
}

func (c Config) Validate() error {
	switch {
	case c.MinDelay < 0:
		return errors.New("min delay must not be negative")
	case c.MaxDelay < c.MinDelay:
		return fmt.Errorf("max delay (%v) must not be below min delay (%v)", c.MaxDelay, c.MinDelay)
	case c.FailRate < 0 || c.FailRate > 1:
		return fmt.Errorf("fail rate %v must be between 0 and 1", c.FailRate)
	}
	return nil
}

// UpdatePublisher sends one task update and returns once the broker has
// confirmed it.
type UpdatePublisher interface {
	PublishUpdate(ctx context.Context, u messaging.TaskUpdate) error
}

// Processor handles one task at a time. Its randomness and sleeping are
// injectable, so tests are deterministic and instant.
type Processor struct {
	cfg    Config
	pub    UpdatePublisher
	logger *slog.Logger
	random func() float64                                   // in [0, 1)
	sleep  func(ctx context.Context, d time.Duration) error // returns ctx.Err() if cancelled
}

func NewProcessor(cfg Config, pub UpdatePublisher, logger *slog.Logger) *Processor {
	return &Processor{
		cfg:    cfg,
		pub:    pub,
		logger: logger,
		random: rand.Float64, //nolint:gosec // simulated outcomes, not security
		sleep:  sleepCtx,
	}
}

// Process runs one task: delay → in_progress → delay → done or failed. Each
// update is confirmed before the next step, and each carries its
// deterministic id, so running a task twice produces the same updates.
func (p *Processor) Process(ctx context.Context, e messaging.TaskEvent) error {
	if err := p.sleep(ctx, p.delay()); err != nil {
		return err
	}
	if err := p.pub.PublishUpdate(ctx, messaging.NewTaskUpdate(e.ID, domain.TaskInProgress, "")); err != nil {
		return fmt.Errorf("publish in_progress: %w", err)
	}

	if err := p.sleep(ctx, p.delay()); err != nil {
		return err
	}
	outcome := domain.TaskDone
	if p.random() < p.cfg.FailRate { // 0 never fails, 1 always does
		outcome = domain.TaskFailed
	}
	if err := p.pub.PublishUpdate(ctx, messaging.NewTaskUpdate(e.ID, outcome, FailureMessage)); err != nil {
		return fmt.Errorf("publish %s: %w", outcome, err)
	}
	p.logger.Info("task processed", "task_id", e.ID, "tenant_id", e.TenantID, "type", e.Type, "outcome", outcome)
	return nil
}

// HandleDelivery processes one delivery and settles it:
//
//   - malformed: nack without requeue, so it goes to worker.tasks.dlq;
//   - done (either outcome): ack, and only now, so a crash before this point
//     means a redelivery and a redo, which the deterministic ids make harmless;
//   - interrupted by shutdown: nack with requeue. That doesn't count toward
//     the delivery limit (see internal/messaging's tests), so a restart costs
//     the task nothing.
//
// A publish failure is returned without settling the delivery. The caller
// stops the worker, the channel closes, and RabbitMQ redelivers the task and
// counts it, so a publish that can never succeed ends in the DLQ instead of
// looping.
func (p *Processor) HandleDelivery(ctx context.Context, d amqp.Delivery) error {
	e, err := messaging.DecodeTaskEvent(d.Body)
	if err != nil {
		p.logger.Error("malformed task; dead-lettering it", "error", err, "message_id", d.MessageId)
		return d.Nack(false, false)
	}

	err = p.Process(ctx, e)
	switch {
	case ctx.Err() != nil:
		p.logger.Info("shutting down; requeueing task", "task_id", e.ID)
		return d.Nack(false, true)
	case err != nil:
		return fmt.Errorf("task %s: %w", e.ID, err)
	}
	return d.Ack(false)
}

func (p *Processor) delay() time.Duration {
	spread := p.cfg.MaxDelay - p.cfg.MinDelay
	if spread <= 0 {
		return p.cfg.MinDelay
	}
	return p.cfg.MinDelay + time.Duration(p.random()*float64(spread+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run consumes worker.tasks until ctx is cancelled (then it returns nil) or
// something fails (then it returns the error, and the process exits to be
// restarted). Each of the Concurrency goroutines has its own publisher
// channel, since a ConfirmPublisher is not safe for concurrent use.
func Run(ctx context.Context, conn *amqp.Connection, cfg Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open consumer channel: %w", err)
	}
	defer ch.Close()
	if err := messaging.DeclareTopology(ch); err != nil {
		return err
	}
	if err := ch.Qos(Concurrency, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	deliveries, err := ch.Consume(messaging.WorkerTasksQueue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", messaging.WorkerTasksQueue, err)
	}
	logger.Info("worker started", "concurrency", Concurrency,
		"min_delay", cfg.MinDelay, "max_delay", cfg.MaxDelay, "fail_rate", cfg.FailRate)

	g, gctx := errgroup.WithContext(ctx)
	for range Concurrency {
		pub, err := NewAMQPUpdatePublisher(conn)
		if err != nil {
			return err
		}
		proc := NewProcessor(cfg, pub, logger)
		g.Go(func() error {
			defer pub.Close()
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
					if err := proc.HandleDelivery(gctx, d); err != nil {
						return err
					}
				}
			}
		})
	}
	err = g.Wait()
	logger.Info("worker stopped")
	return err
}

// AMQPUpdatePublisher publishes task updates to the task-updates exchange.
type AMQPUpdatePublisher struct {
	p *messaging.ConfirmPublisher
}

func NewAMQPUpdatePublisher(conn *amqp.Connection) (*AMQPUpdatePublisher, error) {
	p, err := messaging.NewConfirmPublisher(conn, messaging.TaskUpdatesExchange, messaging.DefaultConfirmTimeout)
	if err != nil {
		return nil, err
	}
	return &AMQPUpdatePublisher{p: p}, nil
}

// PublishUpdate sends u with message_id = update_id and waits for its confirm.
func (a *AMQPUpdatePublisher) PublishUpdate(ctx context.Context, u messaging.TaskUpdate) error {
	body, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("encode update: %w", err)
	}
	id := u.UpdateID.String()
	confirmed, err := a.p.Publish(ctx, []messaging.Message{{
		RoutingKey: messaging.UpdateRoutingKey(u.Status),
		MessageID:  id,
		Type:       messaging.TaskUpdateType,
		Body:       body,
	}})
	if err != nil {
		return err
	}
	if !slices.Contains(confirmed, id) {
		return fmt.Errorf("update %s not confirmed", id)
	}
	return nil
}

func (a *AMQPUpdatePublisher) Close() error {
	return a.p.Close()
}
