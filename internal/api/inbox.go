package api

import (
	"net/http"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// inboxItem is one open condition on the wire. detail carries the kind's own fields; the SPA
// writes the sentence, so no copy lives in the daemon.
type inboxItem struct {
	Kind         string            `json:"kind"`
	Severity     string            `json:"severity"`
	InstanceID   *string           `json:"instance_id"`
	InstanceName string            `json:"instance_name,omitempty"`
	SinceAt      time.Time         `json:"since_at"`
	Detail       map[string]string `json:"detail,omitempty"`
}

// severe names the conditions that risk world data rather than merely needing attention
// (03 §3.4, 12 §3.4).
var severe = map[string]bool{
	alerts.KindLowDisk.String():     true,
	alerts.KindUncleanStop.String(): true,
	alerts.KindCrashLoop.String():   true,
}

// inbox is GET /instances/inbox: every open condition across the instances the caller can see.
//
// Authorized as a collection, the way list() is: VisibleInstances is the filter, and a
// condition whose instance is not in that set never enters the response.
func (h *Instances) inbox(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	ids, all, err := h.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	visible := make(map[string]bool, len(ids))
	for _, id := range ids {
		visible[id] = true
	}

	conditions, err := h.DB.OpenConditions(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	instances, err := h.DB.ListInstances(r.Context(), nil)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	names := make(map[string]string, len(instances))
	for i := range instances {
		names[instances[i].ID] = instances[i].Name
	}

	items := make([]inboxItem, 0, len(conditions))
	for i := range conditions {
		c := &conditions[i]
		if !h.showsCondition(r, u, c, visible, all) {
			continue
		}
		item := inboxItem{
			Kind: c.Kind, Severity: severity(c.Kind), InstanceID: c.InstanceID,
			SinceAt: c.FirstSeenAt, Detail: c.Detail,
		}
		if c.InstanceID != nil {
			item.InstanceName = names[*c.InstanceID]
		}
		items = append(items, item)
	}
	JSON(w, r, http.StatusOK, NewPage(items, nil))
}

// showsCondition reports whether this caller may see one condition. Visibility is the whole
// gate for an instance-scoped one: every grant carries the viewer base role, so a caller who
// can see the instance already holds stats.read and backups.list (09 §3.1).
//
// Low disk is shown to anyone who can see an instance at all, being the same figure /disk gives
// them for their own server on the same filesystem; every other instance-less condition is
// panel-owned work and needs the global action.
func (h *Instances) showsCondition(
	r *http.Request, u *store.User, c *store.AlertCondition, visible map[string]bool, all bool,
) bool {
	if c.InstanceID == nil {
		if c.Kind == alerts.KindLowDisk.String() {
			return all || len(visible) > 0
		}
		return h.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "")
	}
	return all || visible[*c.InstanceID]
}

func severity(kind string) string {
	if severe[kind] {
		return "critical"
	}
	return "warning"
}
