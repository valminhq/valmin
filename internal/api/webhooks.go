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

// Webhooks serves the webhook endpoints in two scopes. The admin routes manage the shared
// webhooks on panel.settings: a webhook is a request the panel makes on the caller's behalf, so
// that is never grantable. A server's routes manage the server's own webhooks on
// notifications.manage and list the shared ones beside them.
type Webhooks struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	Keeper *crypto.Keeper
	Sender *notify.Sender

	dispatcher *deliveryjob.Dispatcher
}

func webhookRoutes(rt *routeTable, h *Webhooks) {
	for _, base := range []string{"/api/v1/admin/webhooks", "/api/v1/instances/{id}/webhooks"} {
		rt.Handle("GET "+base, http.HandlerFunc(h.list))
		rt.Handle("POST "+base, http.HandlerFunc(h.create))
		rt.Handle("PATCH "+base+"/{wid}", http.HandlerFunc(h.update))
		rt.Handle("DELETE "+base+"/{wid}", http.HandlerFunc(h.delete))
		rt.Handle("POST "+base+"/{wid}/test", http.HandlerFunc(h.test))
		rt.Handle("GET "+base+"/deliveries", http.HandlerFunc(h.deliveries))
	}
}

// notifyScope is what a webhook or alert rule request acts for: the panel on the admin routes,
// or one server on its own routes.
type notifyScope struct {
	az   *authz.Authz
	db   *store.DB
	user *store.User
	// instanceID and instanceName are the server, empty on the admin routes.
	instanceID   string
	instanceName string
}

// notifyCaller is the caller and the scope the route names, before any authorization.
func notifyCaller(w http.ResponseWriter, r *http.Request, az *authz.Authz, db *store.DB) (notifyScope, bool) {
	u, ok := caller(w, r)
	if !ok {
		return notifyScope{}, false
	}
	return notifyScope{az: az, db: db, user: u, instanceID: r.PathValue("id")}, true
}

// admit finishes authorizing a request whose Can for the scope's action came out allowed,
// answering the request itself and returning false when it may not go on.
func (s *notifyScope) admit(w http.ResponseWriter, r *http.Request, allowed bool) bool {
	if !allowed {
		s.refuse(w, r)
		return false
	}
	return s.load(w, r)
}

// action is what acting in the scope takes: panel.settings on the admin routes, and
// notifications.manage on the server on a server's.
func (s notifyScope) action() authz.Action {
	if s.instanceID == "" {
		return authz.PanelSettings
	}
	return authz.NotificationsManage
}

// refuse answers a caller who lacks the scope's action: 404 on the admin routes and for a server
// the caller cannot see, as if neither existed, and 403 for a server they can.
func (s notifyScope) refuse(w http.ResponseWriter, r *http.Request) {
	if s.instanceID != "" && s.az.Can(r.Context(), s.user, authz.InstanceView, s.instanceID) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	apierr.Write(w, r, apierr.New(errcode.NotFound))
}

// load reads the server a server's route names, answering 404 itself when there is none.
func (s *notifyScope) load(w http.ResponseWriter, r *http.Request) bool {
	if s.instanceID == "" {
		return true
	}
	inst, err := s.db.InstanceByID(r.Context(), s.instanceID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	s.instanceName = inst.Name
	return true
}

// sees reports whether a webhook or rule belonging to owner, nil when shared or panel-wide, is
// listed in the scope: everything on the admin routes, the shared and the server's own on a
// server's.
func (s notifyScope) sees(owner *string) bool {
	return s.instanceID == "" || owner == nil || *owner == s.instanceID
}

// owns reports whether a webhook or rule belonging to owner may be changed in the scope:
// anything on the admin routes, only the server's own on a server's.
func (s notifyScope) owns(owner *string) bool {
	return s.instanceID == "" || (owner != nil && *owner == s.instanceID)
}

// owner is what a webhook or rule created in the scope belongs to: the server, or nil.
func (s notifyScope) owner() *string {
	if s.instanceID == "" {
		return nil
	}
	return &s.instanceID
}

// audit records a change made in the scope, under the action that allowed it. The change has
// already committed, so a failure is logged rather than returned: refusing the response would
// not undo it.
func (s notifyScope) audit(r *http.Request, detail string) {
	if err := s.db.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: s.user.ID, InstanceID: s.instanceID, Action: s.action().String(), Detail: detail,
		IP: middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		slog.ErrorContext(r.Context(), "audit notification change",
			slog.String("detail", detail), slog.Any("error", err))
	}
}

// webhookView is what a destination looks like from outside. There is no url field, in any
// response, under any role: the URL is the whole authentication for posting to that channel,
// and a value that never leaves the panel cannot be read out of a screenshot or a bug report
// (11 §9). instance_id is the server the webhook belongs to, null when it is shared.
type webhookView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Enabled    bool      `json:"enabled"`
	InstanceID *string   `json:"instance_id"`
	CatchAll   bool      `json:"catch_all"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func toWebhookView(w *store.Webhook) webhookView {
	return webhookView{
		ID: w.ID, Name: w.Name, Kind: w.Kind, Enabled: w.Enabled, InstanceID: w.InstanceID,
		CatchAll: w.CatchAll, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
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

// list answers the webhooks the scope sees.
func (h *Webhooks) list(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	rows, err := h.DB.ListWebhooks(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	writeSeen(w, r, s, rows, func(row *store.Webhook) *string { return row.InstanceID }, toWebhookView)
}

// writeSeen answers with the rows the scope sees, as views. owner is the server a row belongs
// to, nil when it is shared or panel-wide.
func writeSeen[R, V any](
	w http.ResponseWriter, r *http.Request, s notifyScope, rows []R, owner func(*R) *string, view func(*R) V,
) {
	views := make([]V, 0, len(rows))
	for i := range rows {
		if s.sees(owner(&rows[i])) {
			views = append(views, view(&rows[i]))
		}
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

type webhookRequest struct {
	Name     *string `json:"name"`
	Kind     *string `json:"kind"`
	URL      *string `json:"url"`
	Enabled  *bool   `json:"enabled"`
	CatchAll *bool   `json:"catch_all"`
}

// create adds a webhook belonging to the scope: a shared one on the admin routes, the server's
// own on a server's.
func (h *Webhooks) create(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
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
		Enabled: body.Enabled == nil || *body.Enabled, InstanceID: s.owner(),
		CatchAll: body.CatchAll == nil || *body.CatchAll, CreatedBy: &s.user.ID,
	}
	if err := h.DB.CreateWebhook(r.Context(), row); err != nil {
		writeWebhookSaveError(w, r, err)
		return
	}
	h.audit(r, s, "webhook_create", id)
	h.respond(w, r, http.StatusCreated, id)
}

func (h *Webhooks) update(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	existing, ok := h.lookup(w, r, s)
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
	patch := store.WebhookPatch{Name: body.Name, URL: envelope, Enabled: body.Enabled, CatchAll: body.CatchAll}
	if err := h.DB.UpdateWebhook(r.Context(), existing.ID, patch); err != nil {
		writeWebhookSaveError(w, r, err)
		return
	}
	h.audit(r, s, "webhook_update", existing.ID)
	h.respond(w, r, http.StatusOK, existing.ID)
}

// respond answers with the stored webhook id.
func (h *Webhooks) respond(w http.ResponseWriter, r *http.Request, status int, id string) {
	stored, err := h.DB.WebhookByID(r.Context(), id)
	if err != nil || stored == nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, status, toWebhookView(stored))
}

// writeWebhookSaveError answers a failed webhook write: a taken name is the caller's to fix.
func writeWebhookSaveError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrWebhookNameTaken) {
		apierr.Write(w, r, apierr.New(errcode.NameTaken).With("field", "name"))
		return
	}
	apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
}

func (h *Webhooks) delete(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	existing, ok := h.lookup(w, r, s)
	if !ok {
		return
	}
	if err := h.DB.DeleteWebhook(r.Context(), existing.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, s, "webhook_delete", existing.ID)
	w.WriteHeader(http.StatusNoContent)
}

// deliveries lists delivery records newest first, optionally narrowed to one destination, one
// alert rule or one status. The filters travel with every page's request alongside the cursor.
// A server's routes list only the deliveries about that server.
func (h *Webhooks) deliveries(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
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
		InstanceID: s.instanceID,
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
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	existing, ok := h.lookup(w, r, s)
	if !ok {
		return
	}
	event := notify.Event{
		ID:           store.NewID(),
		Kind:         notify.KindTest,
		OccurredAt:   time.Now().UTC(),
		InstanceID:   s.instanceID,
		InstanceName: s.instanceName,
		Detail: []notify.Field{
			{Name: "Requested by", Value: s.user.Username},
			{Name: "Result", Value: "This destination receives Valmin's notifications."},
		},
	}
	job, err := h.dispatcher.Dispatch(r.Context(), existing, &event, s.user.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	h.audit(r, s, "webhook_test", existing.ID)
	Accepted(w, r, job.ID, toJobView(job))
}

// lookup reads the webhook a route names, answering 404 when there is none the scope may change.
func (h *Webhooks) lookup(w http.ResponseWriter, r *http.Request, s notifyScope) (*store.Webhook, bool) {
	row, err := h.DB.WebhookByID(r.Context(), r.PathValue("wid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if row == nil || !s.owns(row.InstanceID) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, false
	}
	return row, true
}

// audit records who changed a destination.
func (h *Webhooks) audit(r *http.Request, s notifyScope, operation, webhookID string) {
	s.audit(r, fmt.Sprintf(`{"operation":%q,"webhook_id":%q}`, operation, webhookID))
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
