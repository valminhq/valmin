package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// maxWebhookName bounds the operator's label for a destination.
const maxWebhookName = 60

// The dispatcher's cadence. It is a pass over rows that already exist, so the interval only
// bounds how late a notification can be when the submit that should have sent it did not run.
const (
	dispatchInterval = 15 * time.Second
	dispatchBatch    = 100
)

// Webhooks serves /admin/webhooks (04 §3, 05 M6). Configuring a destination is a request the
// panel will make on the operator's behalf, which is an SSRF primitive, so it is panel.settings
// — 09 §3.3's never-grantable list — and a member sees the whole group as absent (ADR-038).
type Webhooks struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	Keeper *crypto.Keeper
	Sender *notify.Sender
}

func (h *Webhooks) Routes(rt *Router) {
	rt.Handle("GET /api/v1/admin/webhooks", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/admin/webhooks", http.HandlerFunc(h.create))
	rt.Handle("PATCH /api/v1/admin/webhooks/{id}", http.HandlerFunc(h.update))
	rt.Handle("DELETE /api/v1/admin/webhooks/{id}", http.HandlerFunc(h.delete))
	rt.Handle("POST /api/v1/admin/webhooks/{id}/test", http.HandlerFunc(h.test))
	rt.Handle("GET /api/v1/admin/webhooks/deliveries", http.HandlerFunc(h.deliveries))
}

// webhookView is what a destination looks like from outside. There is no url field, in any
// response, under any role: the URL is the whole authentication for posting to that channel,
// and a value that never leaves the panel cannot be read out of a screenshot or a bug report
// (11 §9).
type webhookView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toWebhookView(w *store.Webhook) webhookView {
	return webhookView{
		ID: w.ID, Name: w.Name, Kind: w.Kind, Enabled: w.Enabled,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

// deliveryView is one destination's copy of one event, with what happened to it. last_error
// is the sanitized transport failure, so it never carries the URL it failed to reach.
type deliveryView struct {
	ID         string    `json:"id"`
	WebhookID  string    `json:"webhook_id"`
	EventID    string    `json:"event_id"`
	EventKind  string    `json:"event_kind"`
	InstanceID *string   `json:"instance_id"`
	Status     string    `json:"status"`
	Attempts   int       `json:"attempts"`
	LastError  *string   `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (h *Webhooks) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	rows, err := h.DB.ListWebhooks(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	views := make([]webhookView, 0, len(rows))
	for i := range rows {
		views = append(views, toWebhookView(&rows[i]))
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

type webhookRequest struct {
	Name    *string `json:"name"`
	Kind    *string `json:"kind"`
	URL     *string `json:"url"`
	Enabled *bool   `json:"enabled"`
}

func (h *Webhooks) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	var body webhookRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var v apierr.Validation
	name := deref(body.Name)
	kind := deref(body.Kind)
	validateWebhookName(&v, name)
	if !slices.Contains(notify.DestinationKinds, kind) {
		v.Add("kind", apierr.FieldNotAnOption, "kind must be discord or generic.")
	}
	if body.URL == nil || *body.URL == "" {
		v.Add("url", apierr.FieldRequired, "A destination URL is required.")
	}
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// Resolved here rather than only at send time, so an operator learns a destination is
	// unreachable while they are looking at the form. The send resolves again regardless:
	// a name that answered publicly once is not a promise about the next answer.
	if _, err := h.Sender.Resolve(r.Context(), *body.URL); err != nil {
		writeAddressError(w, r, err)
		return
	}

	id := store.NewID()
	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeWebhookURL, crypto.WebhookURLLocation(id), []byte(*body.URL))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	row := &store.Webhook{
		ID: id, Name: name, Kind: kind, URL: envelope,
		Enabled: body.Enabled == nil || *body.Enabled, CreatedBy: &u.ID,
	}
	if err := h.DB.CreateWebhook(r.Context(), row); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "name"))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_create", id)

	stored, err := h.DB.WebhookByID(r.Context(), id)
	if err != nil || stored == nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toWebhookView(stored))
}

func (h *Webhooks) update(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	existing, ok := h.lookup(w, r)
	if !ok {
		return
	}
	var body webhookRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var v apierr.Validation
	if body.Kind != nil {
		// The stored payload shape belongs to the destination kind. Changing it would
		// reinterpret the deliveries already recorded against this row, so it is a new
		// destination rather than an edit.
		v.Add("kind", apierr.FieldInvalid, "A destination's kind cannot be changed.")
	}
	if body.Name != nil {
		validateWebhookName(&v, *body.Name)
	}
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	var envelope *string
	if body.URL != nil {
		if _, err := h.Sender.Resolve(r.Context(), *body.URL); err != nil {
			writeAddressError(w, r, err)
			return
		}
		sealed, err := h.Keeper.Encrypt(
			crypto.PurposeWebhookURL, crypto.WebhookURLLocation(existing.ID), []byte(*body.URL))
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		envelope = &sealed
	}
	if err := h.DB.UpdateWebhook(r.Context(), existing.ID, body.Name, envelope, body.Enabled); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "name"))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_update", existing.ID)

	stored, err := h.DB.WebhookByID(r.Context(), existing.ID)
	if err != nil || stored == nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toWebhookView(stored))
}

func (h *Webhooks) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	existing, ok := h.lookup(w, r)
	if !ok {
		return
	}
	if err := h.DB.DeleteWebhook(r.Context(), existing.ID); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_delete", existing.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Webhooks) deliveries(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	limit, err := ParseLimit(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	cursor, hasCursor, err := ParseCursor(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	var afterTime, afterID string
	if hasCursor {
		afterTime, afterID = cursor.SortKey, cursor.ID
	}
	rows, err := h.DB.ListDeliveries(r.Context(), afterTime, afterID, limit)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	views := make([]deliveryView, 0, len(rows))
	for i := range rows {
		d := &rows[i]
		views = append(views, deliveryView{
			ID: d.ID, WebhookID: d.WebhookID, EventID: d.EventID, EventKind: d.EventKind,
			InstanceID: d.InstanceID, Status: d.Status, Attempts: d.Attempts,
			LastError: d.LastError, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
	}
	var next *string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: store.FormatTime(last.CreatedAt), ID: last.ID}.Encode()
		next = &encoded
	}
	JSON(w, r, http.StatusOK, NewPage(views, next))
}

// test sends one real notification down the real path, so what an operator verifies is the
// address policy and the destination's credential rather than a form validator.
func (h *Webhooks) test(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	existing, ok := h.lookup(w, r)
	if !ok {
		return
	}
	event := notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindTest,
		OccurredAt: time.Now().UTC(),
		Detail:     map[string]string{"Requested by": u.Username},
	}
	job, err := h.Dispatch(r.Context(), existing, &event, u.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	h.audit(r, u.ID, "webhook_test", existing.ID)
	Accepted(w, r, job.ID, toJobView(job))
}

// Dispatch records one delivery intent and submits the job that sends it, for a caller that
// needs the job back — the test send, which is one destination and a job id in the response.
// Every other event goes through Emit.
func (h *Webhooks) Dispatch(
	ctx context.Context, destination *store.Webhook, event *notify.Event, requestedBy string,
) (*store.Job, error) {
	delivery, err := renderDelivery(destination, event)
	if err != nil {
		return nil, err
	}
	job, err := h.Engine.Submit(ctx, deliverySpec(delivery, requestedBy, func(ctx context.Context, tx *sql.Tx) error {
		return store.TxCreateDelivery(ctx, tx, delivery)
	}), h.runDelivery(delivery.ID, destination.ID))
	if err != nil {
		return nil, fmt.Errorf("submit delivery: %w", err)
	}
	return job, nil
}

// Prepare renders one delivery row per enabled destination. Nothing is sent by it: the rows
// are the delivery intent, and the caller writes them with the change that caused the event.
func (h *Webhooks) Prepare(ctx context.Context, event *notify.Event) ([]*store.Delivery, error) {
	destinations, err := h.DB.EnabledWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	out := make([]*store.Delivery, 0, len(destinations))
	for i := range destinations {
		delivery, err := renderDelivery(&destinations[i], event)
		if err != nil {
			return nil, err
		}
		out = append(out, delivery)
	}
	return out, nil
}

// TxRecordDeliveries writes prepared intents inside the caller's transaction, so a notification
// is owed exactly when the change that owes it commits.
func TxRecordDeliveries(ctx context.Context, tx *sql.Tx, deliveries []*store.Delivery) error {
	for _, d := range deliveries {
		if err := store.TxCreateDelivery(ctx, tx, d); err != nil {
			return fmt.Errorf("record delivery intent: %w", err)
		}
	}
	return nil
}

// Emit is the path for an event whose source change is not a transaction this package holds:
// the rows are written and the dispatcher sends them. A failure is logged and nothing else —
// no notification ever changes the outcome it reports (05 M6).
func (h *Webhooks) Emit(ctx context.Context, event *notify.Event) {
	deliveries, err := h.Prepare(ctx, event)
	if err != nil {
		slog.ErrorContext(ctx, "prepare notification",
			slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
		return
	}
	for _, d := range deliveries {
		if err := h.DB.CreateDelivery(ctx, d); err != nil {
			slog.ErrorContext(ctx, "record delivery intent",
				slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
			return
		}
	}
	h.Send(ctx, deliveries)
}

// Send submits the delivery job for rows already written. A row whose job cannot be submitted
// stays pending and is picked up by the next dispatcher pass, so nothing is lost by failing
// here.
func (h *Webhooks) Send(ctx context.Context, deliveries []*store.Delivery) {
	for _, d := range deliveries {
		if _, err := h.Engine.Submit(
			ctx, deliverySpec(d, "", nil), h.runDelivery(d.ID, d.WebhookID),
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
func (h *Webhooks) Run(ctx context.Context) {
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

func (h *Webhooks) dispatch(ctx context.Context) {
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

// renderDelivery builds one destination's copy of one event.
func renderDelivery(destination *store.Webhook, event *notify.Event) (*store.Delivery, error) {
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

// deliverySpec is one send's job. The lock key is the delivery row, so two destinations
// receiving the same event do not queue behind each other and one row is never sent twice at
// once. The payload names the row and never the destination: a credential copied into a job
// payload is a credential in every job listing (11 §9).
func deliverySpec(
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
func (h *Webhooks) runDelivery(deliveryID, webhookID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		fail := func(code apierr.Code, err error) jobs.Outcome {
			_ = h.DB.FinishDelivery(ctx, deliveryID, store.DeliveryFailed, 0, err.Error())
			return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: code.String(), Error: err.Error()}
		}

		delivery, err := h.DB.DeliveryByID(ctx, deliveryID)
		if err != nil || delivery == nil {
			return fail(apierr.Internal, fmt.Errorf("read delivery %s: %w", deliveryID, err))
		}
		destination, err := h.DB.WebhookByID(ctx, webhookID)
		if err != nil {
			return fail(apierr.Internal, fmt.Errorf("read destination: %w", err))
		}
		if destination == nil {
			return fail(apierr.NotFound, errors.New("the destination was removed before the send"))
		}
		url, err := h.Keeper.Decrypt(
			crypto.PurposeWebhookURL, crypto.WebhookURLLocation(destination.ID), destination.URL)
		if err != nil {
			return fail(apierr.Internal, fmt.Errorf("read destination URL: %w", err))
		}

		jh.Progress(ctx, 10, "Sending to "+destination.Name)
		attempts, sendErr := h.Sender.Send(ctx, string(url), notify.Body{
			ContentType: notify.ContentType, Bytes: []byte(delivery.Payload),
		})
		if sendErr != nil {
			// Sanitized by the sender: nothing here quotes the URL it could not reach.
			message := fmt.Sprintf("%v (%d attempts)", sendErr, attempts)
			if err := h.DB.FinishDelivery(
				ctx, deliveryID, store.DeliveryFailed, attempts, message); err != nil {
				return fail(apierr.Internal, err)
			}
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: apierr.Unavailable.String(), Error: message,
			}
		}
		if err := h.DB.FinishDelivery(
			ctx, deliveryID, store.DeliveryDelivered, attempts, ""); err != nil {
			return fail(apierr.Internal, err)
		}
		jh.Progress(ctx, 100, fmt.Sprintf("Delivered to %s after %d attempt(s)", destination.Name, attempts))
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

func (h *Webhooks) lookup(w http.ResponseWriter, r *http.Request) (*store.Webhook, bool) {
	row, err := h.DB.WebhookByID(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, false
	}
	return row, true
}

// audit records who changed a destination. The change has already committed, so a failure
// here is logged rather than returned: refusing the response would not undo it.
func (h *Webhooks) audit(r *http.Request, userID, operation, webhookID string) {
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: userID, Action: authz.PanelSettings.String(),
		Detail: fmt.Sprintf(`{"operation":%q,"webhook_id":%q}`, operation, webhookID),
		IP:     middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		slog.ErrorContext(r.Context(), "audit webhook change",
			slog.String("operation", operation), slog.Any("error", err))
	}
}

// writeAddressError turns a refused destination into a field error rather than a transport
// failure: the URL is what the operator has to change.
func writeAddressError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, notify.ErrRejectedAddress) {
		var v apierr.Validation
		v.Add("url", apierr.FieldInvalid, addressMessage(err))
		apierr.Write(w, r, v.Err())
		return
	}
	var v apierr.Validation
	v.Add("url", apierr.FieldInvalid, "That address could not be resolved.")
	apierr.Write(w, r, v.Err())
}

func addressMessage(err error) string {
	return "That destination is not allowed: " + err.Error()
}

func validateWebhookName(v *apierr.Validation, name string) {
	switch {
	case name == "":
		v.Add("name", apierr.FieldRequired, "A name is required.")
	case len(name) > maxWebhookName:
		v.Add("name", apierr.FieldInvalid,
			fmt.Sprintf("A name is at most %d characters.", maxWebhookName))
	}
}
