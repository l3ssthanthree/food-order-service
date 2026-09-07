package outbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/avito-intern/kitchen/internal/postgres"
)

type Store interface {
	ClaimOutbox(context.Context) (postgres.OutboxEvent, bool, error)
	MarkOutboxDelivered(context.Context, string, int) error
	MarkOutboxFailed(context.Context, string, int, string, time.Time) error
}

type Dispatcher struct {
	store         Store
	client        *http.Client
	callbackToken string
	pollInterval  time.Duration
	logger        *slog.Logger
}

func NewDispatcher(store Store, token string, pollInterval time.Duration, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:         store,
		client:        &http.Client{Timeout: 5 * time.Second},
		callbackToken: token,
		pollInterval:  pollInterval,
		logger:        logger,
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.dispatchOne(ctx)
		}
	}
}

func (d *Dispatcher) dispatchOne(ctx context.Context) {
	event, found, err := d.store.ClaimOutbox(ctx)
	if err != nil {
		d.logger.Error("claim outbox event", "error", err)
		return
	}
	if !found {
		return
	}
	if err := d.deliver(ctx, event); err != nil {
		retryAt := time.Now().Add(retryDelay(event.Attempts))
		if markErr := d.store.MarkOutboxFailed(ctx, event.ID, event.Attempts, err.Error(), retryAt); markErr != nil {
			d.logger.Error("reschedule outbox event", "event_id", event.ID, "error", markErr)
		}
		d.logger.Warn("outbox delivery failed", "event_id", event.ID, "attempt", event.Attempts, "error", err)
		return
	}
	if err := d.store.MarkOutboxDelivered(ctx, event.ID, event.Attempts); err != nil {
		d.logger.Error("mark outbox delivered", "event_id", event.ID, "error", err)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, event postgres.OutboxEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, event.DestinationURL, bytes.NewReader(event.Payload))
	if err != nil {
		return fmt.Errorf("create callback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.callbackToken)
	response, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("send callback: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("callback returned %d: %s", response.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}
