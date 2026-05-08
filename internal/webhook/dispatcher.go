// Package webhook implements async webhook delivery for agent endpoints.
// When a message arrives in an agent's mailbox and the agent has a non-empty
// Endpoint URL, the Dispatcher POSTs a signed JSON payload to that URL with
// automatic exponential-backoff retries.
package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/rbaliyan/event/v3"
	"github.com/rbaliyan/mailbox"
	"github.com/rbaliyan/postbox/internal/store"
)

// DeliveryStore persists and queries delivery job state.
type DeliveryStore interface {
	SaveDeliveryJob(ctx context.Context, job store.DeliveryJob) error
	UpdateDeliveryJob(ctx context.Context, job store.DeliveryJob) error
	GetDeliveryJob(ctx context.Context, messageID, recipientID string) (store.DeliveryJob, error)
	ListPendingDeliveryJobs(ctx context.Context, before time.Time, limit int) ([]store.DeliveryJob, error)
}

// Dispatcher subscribes to mailbox MessageReceived events and delivers
// webhook POSTs to registered user endpoints (metadata["endpoint"]).
type Dispatcher struct {
	users   mailbox.UserResolver
	jobs    DeliveryStore
	mailbox mailbox.Service
	signer  *Signer
	client  *http.Client
	logger  *slog.Logger

	workers     int
	maxAttempts int
	sweepPeriod time.Duration

	queue  chan store.DeliveryJob
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a Dispatcher. Call Start to begin processing.
func New(users mailbox.UserResolver, jobs DeliveryStore, mbx mailbox.Service, signer *Signer, opts ...Option) *Dispatcher {
	cfg := options{
		workers:     4,
		maxAttempts: defaultMaxTry,
		sweepPeriod: 30 * time.Second,
		client:      &http.Client{Timeout: 10 * time.Second},
		logger:      slog.Default(),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &Dispatcher{
		users:       users,
		jobs:        jobs,
		mailbox:     mbx,
		signer:      signer,
		client:      cfg.client,
		logger:      cfg.logger,
		workers:     cfg.workers,
		maxAttempts: cfg.maxAttempts,
		sweepPeriod: cfg.sweepPeriod,
		queue:       make(chan store.DeliveryJob, cfg.workers*10),
		stopCh:      make(chan struct{}),
	}
}

// Start subscribes to MessageReceived events and launches worker goroutines.
func (d *Dispatcher) Start(ctx context.Context) error {
	if err := d.mailbox.Events().MessageReceived.Subscribe(ctx, d.onMessageReceived); err != nil {
		return fmt.Errorf("webhook dispatcher: subscribe: %w", err)
	}
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go d.worker(ctx)
	}
	d.wg.Add(1)
	go d.sweeper(ctx)
	return nil
}

// Stop signals workers to drain and waits for them to finish.
func (d *Dispatcher) Stop() {
	close(d.stopCh)
	d.wg.Wait()
}

// onMessageReceived handles a per-recipient delivery event from the mailbox.
func (d *Dispatcher) onMessageReceived(ctx context.Context, _ event.Event[mailbox.MessageReceivedEvent], data mailbox.MessageReceivedEvent) error {
	user, err := d.users.ResolveUser(ctx, data.RecipientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // user not registered, nothing to do
	}
	if err != nil {
		return fmt.Errorf("webhook: lookup user %q: %w", data.RecipientID, err)
	}
	endpoint := user.Capabilities()["endpoint"]
	if endpoint == "" {
		return nil // no webhook endpoint configured
	}

	job := store.DeliveryJob{
		MessageID:   data.MessageID,
		RecipientID: data.RecipientID,
		EndpointURL: endpoint,
		Status:      store.DeliveryPending,
		MaxAttempts: d.maxAttempts,
		NextRetryAt: time.Now().UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	if err := d.jobs.SaveDeliveryJob(ctx, job); err != nil {
		d.logger.Error("webhook: save delivery job", "error", err,
			"message_id", data.MessageID, "recipient", data.RecipientID)
		return nil // don't propagate; sweeper will pick it up
	}

	select {
	case d.queue <- job:
	default:
		// Queue full — sweeper will pick up the persisted job.
		d.logger.Warn("webhook: queue full, job will be retried by sweeper",
			"message_id", data.MessageID, "recipient", data.RecipientID)
	}
	return nil
}

// worker drains the queue channel and attempts delivery.
func (d *Dispatcher) worker(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-d.stopCh:
			return
		case job := <-d.queue:
			d.attempt(ctx, job)
		}
	}
}

// sweeper periodically re-enqueues jobs that are due for retry but were
// never enqueued (e.g. on restart or when the queue was full).
func (d *Dispatcher) sweeper(ctx context.Context) {
	defer d.wg.Done()
	t := time.NewTicker(d.sweepPeriod)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			jobs, err := d.jobs.ListPendingDeliveryJobs(ctx, time.Now().UTC(), d.workers*10)
			if err != nil {
				d.logger.Error("webhook: sweeper list jobs", "error", err)
				continue
			}
			for _, j := range jobs {
				select {
				case d.queue <- j:
				default:
				}
			}
		}
	}
}

// attempt performs one HTTP POST for a delivery job and updates its state.
func (d *Dispatcher) attempt(ctx context.Context, job store.DeliveryJob) {
	msg, err := d.mailbox.Client(job.RecipientID).Get(ctx, job.MessageID)
	if err != nil {
		d.logger.Error("webhook: fetch message", "error", err,
			"message_id", job.MessageID, "recipient", job.RecipientID)
		return
	}

	payload := BuildPayload(job.RecipientID, msg, time.Now().UTC())
	body, err := payload.Marshal()
	if err != nil {
		d.logger.Error("webhook: marshal payload", "error", err)
		return
	}

	now := time.Now().UTC()
	headers := d.signer.SignedHeaders(body, now)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.EndpointURL, bytes.NewReader(body))
	if err != nil {
		d.fail(ctx, job, fmt.Sprintf("build request: %v", err), false)
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	job.Attempts++

	if err != nil {
		d.fail(ctx, job, err.Error(), false)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t := time.Now().UTC()
		job.Status = store.DeliveryDelivered
		job.DeliveredAt = &t
		job.LastError = ""
		if err := d.jobs.UpdateDeliveryJob(ctx, job); err != nil {
			d.logger.Error("webhook: update delivered job", "error", err)
		}
		d.logger.Info("webhook: delivered", "message_id", job.MessageID,
			"recipient", job.RecipientID, "attempts", job.Attempts)
		return
	}

	d.fail(ctx, job, fmt.Sprintf("HTTP %d", resp.StatusCode), isPermanentFailure(resp.StatusCode))
}

// fail records a delivery failure and schedules a retry or marks the job dead.
func (d *Dispatcher) fail(ctx context.Context, job store.DeliveryJob, errMsg string, permanent bool) {
	job.LastError = errMsg
	if permanent || job.Attempts >= job.MaxAttempts {
		job.Status = store.DeliveryDead
		d.logger.Warn("webhook: dead letter", "message_id", job.MessageID,
			"recipient", job.RecipientID, "attempts", job.Attempts, "error", errMsg)
	} else {
		job.Status = store.DeliveryFailed
		job.NextRetryAt = NextRetryAt(job.Attempts)
		d.logger.Info("webhook: retry scheduled", "message_id", job.MessageID,
			"recipient", job.RecipientID, "attempts", job.Attempts,
			"next_retry", job.NextRetryAt.Format(time.RFC3339))
	}
	if err := d.jobs.UpdateDeliveryJob(ctx, job); err != nil {
		d.logger.Error("webhook: update failed job", "error", err)
	}
}
