package api

import (
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
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	deliveryjob "github.com/valminhq/valmin/internal/notify/delivery"
	"github.com/valminhq/valmin/internal/store"
)

// maxWebhookName bounds the operator's label for a destination.
const maxWebhookName = 60

// Webhooks serves /admin/webhooks (04 §3, 05 M6). Configuring a destination is a request the
// panel will make on the operator's behalf, which is an SSRF primitive, so it is panel.settings
// — 09 §3.3's never-grantable list — and a member sees the whole group as absent (ADR-038).
type Webhooks struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	Keeper *crypto.Keeper
	Sender *notify.Sender

	dispatcher *deliveryjob.Dispatcher
}

func webhookRoutes(rt *routeTable, h *Webhooks) {
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
	RuleID     *string   `json:"rule_id"`
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	rows, err := h.DB.ListWebhooks(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	row := &store.Webhook{
		ID: id, Name: name, Kind: kind, URL: envelope,
		Enabled: body.Enabled == nil || *body.Enabled, CreatedBy: &u.ID,
	}
	if err := h.DB.CreateWebhook(r.Context(), row); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			apierr.Write(w, r, apierr.New(errcode.NameTaken).With("field", "name"))
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_create", id)

	stored, err := h.DB.WebhookByID(r.Context(), id)
	if err != nil || stored == nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		envelope = &sealed
	}
	if err := h.DB.UpdateWebhook(r.Context(), existing.ID, body.Name, envelope, body.Enabled); err != nil {
		if errors.Is(err, store.ErrWebhookNameTaken) {
			apierr.Write(w, r, apierr.New(errcode.NameTaken).With("field", "name"))
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_update", existing.ID)

	stored, err := h.DB.WebhookByID(r.Context(), existing.ID)
	if err != nil || stored == nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	existing, ok := h.lookup(w, r)
	if !ok {
		return
	}
	if err := h.DB.DeleteWebhook(r.Context(), existing.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "webhook_delete", existing.ID)
	w.WriteHeader(http.StatusNoContent)
}

// deliveries lists delivery records newest first, optionally narrowed to one destination, one
// alert rule or one status. The filters travel with every page's request alongside the cursor.
func (h *Webhooks) deliveries(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
	q := r.URL.Query()
	filter := store.DeliveryFilter{
		WebhookID: q.Get("webhook_id"), RuleID: q.Get("rule_id"), Status: q.Get("status"),
	}
	statuses := []string{store.DeliveryPending, store.DeliveryDelivered, store.DeliveryFailed}
	if filter.Status != "" && !slices.Contains(statuses, filter.Status) {
		var v apierr.Validation
		v.Add("status", apierr.FieldNotAnOption, "status must be pending, delivered or failed.")
		apierr.Write(w, r, v.Err())
		return
	}
	var afterTime, afterID string
	if hasCursor {
		afterTime, afterID = cursor.SortKey, cursor.ID
	}
	// One more than asked for, so the page knows there is a next one without a COUNT.
	rows, err := h.DB.ListDeliveries(r.Context(), filter, afterTime, afterID, limit+1)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: store.FormatTime(last.CreatedAt), ID: last.ID}.Encode()
		next = &encoded
	}
	views := make([]deliveryView, 0, len(rows))
	for i := range rows {
		d := &rows[i]
		views = append(views, deliveryView{
			ID: d.ID, WebhookID: d.WebhookID, EventID: d.EventID, EventKind: d.EventKind,
			InstanceID: d.InstanceID, RuleID: d.RuleID, Status: d.Status, Attempts: d.Attempts,
			LastError: d.LastError, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
	job, err := h.dispatcher.Dispatch(r.Context(), existing, &event, u.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	h.audit(r, u.ID, "webhook_test", existing.ID)
	Accepted(w, r, job.ID, toJobView(job))
}

func (h *Webhooks) lookup(w http.ResponseWriter, r *http.Request) (*store.Webhook, bool) {
	row, err := h.DB.WebhookByID(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
