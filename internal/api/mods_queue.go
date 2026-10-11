package api

import (
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/store"
)

// queuedModView is one install waiting for its server to stop.
type queuedModView struct {
	FullName  string `json:"full_name"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

// modQueueView is GET /instances/{id}/mods/queue: the installs in the order they will run.
type modQueueView struct {
	Queued []queuedModView `json:"queued"`
}

func toQueuedModView(q *store.QueuedModInstall) queuedModView {
	return queuedModView{
		FullName: q.FullName, Version: q.Version, Source: q.Source,
		CreatedAt: q.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toModQueueView(queued []store.QueuedModInstall) modQueueView {
	view := modQueueView{Queued: make([]queuedModView, 0, len(queued))}
	for i := range queued {
		view.Queued = append(view.Queued, toQueuedModView(&queued[i]))
	}
	return view
}

// listModQueue handles GET /instances/{id}/mods/queue, gated on mods.list.
func (m *Mods) listModQueue(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	queued, err := m.DB.QueuedModInstalls(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toModQueueView(queued))
}

// queueModInstall handles POST /instances/{id}/mods/queue: the install body of POST
// /instances/{id}/mods, run once the server is stopped. A package already queued is replaced.
func (m *Mods) queueModInstall(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	body, ok := decodePackageRequest(w, r)
	if !ok {
		return
	}
	q := &store.QueuedModInstall{
		InstanceID: id, FullName: body.FullName, Version: body.Version, Source: body.Source,
		RequestedBy: u.ID,
	}
	if err := m.DB.QueueModInstall(r.Context(), q); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	q.CreatedAt = time.Now()
	JSON(w, r, http.StatusCreated, toQueuedModView(q))
}

// queueModUpdates handles POST /instances/{id}/mods/updates/queue: the confirmed targets of
// POST /instances/{id}/mods/updates, queued together for the next stop or restart. It answers
// with the whole queue.
func (m *Mods) queueModUpdates(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body applyUpdatesRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var val apierr.Validation
	targets, err := m.checkUpdateTargets(r.Context(), id, body.Targets, &val)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}
	entries := make([]store.QueuedModInstall, len(targets))
	for i, t := range targets {
		entries[i] = store.QueuedModInstall{
			InstanceID: id, FullName: t.FullName, Version: t.Version, Source: t.Source, RequestedBy: u.ID,
		}
	}
	if err := m.DB.QueueModInstalls(r.Context(), entries); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	queued, err := m.DB.QueuedModInstalls(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toModQueueView(queued))
}

// unqueueModInstall handles DELETE /instances/{id}/mods/queue/{full_name}.
func (m *Mods) unqueueModInstall(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	removed, err := m.DB.UnqueueModInstall(r.Context(), id, r.PathValue("full_name"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !removed {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
