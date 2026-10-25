package api

import (
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/reachability"
)

// reachabilityHTTPTimeout bounds each outbound request of a reachability check.
const reachabilityHTTPTimeout = 10 * time.Second

// reachability is GET /instances/{id}/reachability: whether players outside the host can find
// the server, measured from the panel host. Every call makes outbound requests, so each user
// has a request budget. Anyone who can see the server may ask, since they need its address to
// join. A check that finds a problem still answers 200, with the problem in the report.
func (h *Instances) reachability(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if allowed, retry := h.reachabilityLimit.Allow(u.ID); !allowed {
		middleware.RefuseRateLimited(w, r, retry)
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}

	target := reachability.Target{
		BasePort:  inst.BasePort,
		Public:    inst.Public,
		Crossplay: inst.Crossplay,
		Running:   instance.State(inst.State) == instance.StateRunning,

		RestartPending: inst.RestartRequired,
	}
	if reader := h.Streams.Reader(id); reader != nil {
		target.JoinCode = reader.JoinCode()
	}
	JSON(w, r, http.StatusOK, h.Reachability.Check(r.Context(), target))
}
