package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

const (
	dispatchInterval = 15 * time.Second
	dispatchBatch    = 100
)

// Dispatcher submits and runs durable webhook deliveries.
type Dispatcher struct {
	DB     *store.DB
	Engine *jobs.Engine
	Keeper *crypto.Keeper
	Sender func() *notify.Sender
}

// Dispatch records one delivery intent and submits its send job.
func (h *Dispatcher) Dispatch(
	ctx context.Context, destination *store.Webhook, event *notify.Event, requestedBy string,
) (*store.Job, error) {
	delivery, err := Render(destination, event)
	if err != nil {
		return nil, err
	}
	job, err := h.Engine.Submit(ctx, Spec(delivery, requestedBy, func(ctx context.Context, tx *sql.Tx) error {
		return store.TxCreateDelivery(ctx, tx, delivery)
	}), h.RunDelivery(delivery.ID, destination.ID))
	if err != nil {
		return nil, fmt.Errorf("submit delivery: %w", err)
	}
	return job, nil
}

// Send submits the delivery job for rows already written. A row whose job cannot be submitted
// stays pending and is picked up by the next dispatcher pass, so nothing is lost by failing
// here.
func (h *Dispatcher) Send(ctx context.Context, deliveries []*store.Delivery) {
	for _, d := range deliveries {
		if _, err := h.Engine.Submit(
			ctx, Spec(d, "", nil), h.RunDelivery(d.ID, d.WebhookID),
		); err != nil {
			var conflict *store.JobConflict
			if errors.As(err, &conflict) {
				// Its send is already running. Not an error: the lock is the dedupe.
				continue
			}
			slog.WarnContext(ctx, "submit delivery",
				slog.String("delivery_id", d.ID), slog.Any("error", err))
		}
	}
}

// Run is the dispatcher: it submits the send for every delivery intent that has not reached a
// terminal status, including one whose job the panel died holding. Delivery is at-least-once,
// so a row whose remote accepted a request the panel never saw the answer to is sent again
// (12 §9.4).
func (h *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(dispatchInterval)
	defer ticker.Stop()
	for {
		h.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Dispatcher) dispatch(ctx context.Context) {
	pending, err := h.DB.ListPendingDeliveries(ctx, dispatchBatch)
	if err != nil {
		slog.WarnContext(ctx, "read pending deliveries", slog.Any("error", err))
		return
	}
	rows := make([]*store.Delivery, 0, len(pending))
	for i := range pending {
		rows = append(rows, &pending[i])
	}
	h.Send(ctx, rows)
}

// deliverySpec is one send's job. The lock key is the delivery row, so two destinations
// receiving the same event do not queue behind each other and one row is never sent twice at
// once. The payload names the row and never the destination: a credential copied into a job
// payload is a credential in every job listing (11 §9).
func Spec(
	d *store.Delivery, requestedBy string, onClaim func(context.Context, *sql.Tx) error,
) *jobs.Spec {
	return &jobs.Spec{
		Kind:        jobs.KindWebhookDeliver,
		LockKey:     "webhook_delivery:" + d.ID,
		Payload:     map[string]string{"delivery_id": d.ID},
		RequestedBy: requestedBy,
		OnClaim:     onClaim,
	}
}

// runDelivery posts one payload. The URL is decrypted here, inside the job, and is never
// held by the job row, the handler's response or a log line.
func (h *Dispatcher) RunDelivery(deliveryID, webhookID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		fail := func(code errcode.Code, err error) jobs.Outcome {
			_ = h.DB.FinishDelivery(ctx, deliveryID, store.DeliveryFailed, 0, err.Error())
			return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: code.String(), Error: err.Error()}
		}

		delivery, err := h.DB.DeliveryByID(ctx, deliveryID)
		if err != nil || delivery == nil {
			return fail(errcode.Internal, fmt.Errorf("read delivery %s: %w", deliveryID, err))
		}
		destination, err := h.DB.WebhookByID(ctx, webhookID)
		if err != nil {
			return fail(errcode.Internal, fmt.Errorf("read destination: %w", err))
		}
		if destination == nil {
			return fail(errcode.NotFound, errors.New("the destination was removed before the send"))
		}
		url, err := h.Keeper.Decrypt(
			crypto.PurposeWebhookURL, crypto.WebhookURLLocation(destination.ID), destination.URL)
		if err != nil {
			return fail(errcode.Internal, fmt.Errorf("read destination URL: %w", err))
		}

		jh.Progress(ctx, 10, "Sending to "+destination.Name)
		attempts, sendErr := h.Sender().Send(ctx, string(url), notify.Body{
			ContentType: notify.ContentType, Bytes: []byte(delivery.Payload),
		})
		if sendErr != nil {
			// Sanitized by the sender: nothing here quotes the URL it could not reach.
			message := fmt.Sprintf("%v (%d attempts)", sendErr, attempts)
			if err := h.DB.FinishDelivery(
				ctx, deliveryID, store.DeliveryFailed, attempts, message); err != nil {
				return fail(errcode.Internal, err)
			}
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Unavailable.String(), Error: message,
			}
		}
		if err := h.DB.FinishDelivery(
			ctx, deliveryID, store.DeliveryDelivered, attempts, ""); err != nil {
			return fail(errcode.Internal, err)
		}
		jh.Progress(ctx, 100, fmt.Sprintf("Delivered to %s after %d attempt(s)", destination.Name, attempts))
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

// Record writes prepared intents inside the caller's transaction, so a notification
// is owed exactly when the change that owes it commits.
func Record(ctx context.Context, tx *sql.Tx, deliveries []*store.Delivery) error {
	for _, d := range deliveries {
		if err := store.TxCreateDelivery(ctx, tx, d); err != nil {
			return fmt.Errorf("record delivery intent: %w", err)
		}
	}
	return nil
}

// Render builds one destination's copy of one event.
func Render(destination *store.Webhook, event *notify.Event) (*store.Delivery, error) {
	body, err := notify.Render(destination.Kind, event)
	if err != nil {
		return nil, fmt.Errorf("render %s notification: %w", destination.Kind, err)
	}
	d := &store.Delivery{
		ID: store.NewID(), WebhookID: destination.ID, EventID: event.ID,
		EventKind: event.Kind.String(), Payload: string(body.Bytes),
	}
	if event.InstanceID != "" {
		d.InstanceID = &event.InstanceID
	}
	return d, nil
}
