package api

import (
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// publicStatusPerMinute and publicStatusBurst are this route's own per-IP limit, tighter than
// the chain-wide one: it is the only route on this panel an unauthenticated stranger can
// reach, and refreshing a status page is a handful of requests a minute.
const (
	publicStatusPerMinute = 60
	publicStatusBurst     = 20
)

// PublicStatus serves the opt-in, unauthenticated status of one instance (05 M5).
//
// It is the one handler on this panel that does not call Can(), because it has no session and
// therefore no user to pass it. The authorization moved into the data: the query answers only
// for a row whose status_published is true, and answers 404 for everything else — an instance
// that does not exist and one that has not opted in are the same response (ADR-156, ADR-038).
type PublicStatus struct {
	DB      *store.DB
	Streams *instance.Streams
}

// Routes registers the route on mux behind its own thin chain, outside the API chain and
// outside the API subtree. It is not registered on the bare mux the health probes use: 11 §10
// exempts those from rate limiting and security headers, which is fine for a liveness probe on
// a LAN and not for an internet-facing route.
func (h *PublicStatus) Routes(mux *http.ServeMux, chain []middleware.Layer) {
	mux.Handle("GET /public/status/{id}", middleware.Apply(http.HandlerFunc(h.status), chain))
}

// publicStatusView is the whole public contract, and it is short on purpose: the page answers
// "is it up", "who is on" and what the administrator chose to say. Nothing else — no ports, no
// join code, no resource usage, no world name, no state beyond up, down, and a coarse activity.
// Every field here is a disclosure to strangers.
type publicStatusView struct {
	Name   string `json:"name"`
	Online bool   `json:"online"`
	// Players is null when the panel cannot say, exactly as it is on the authenticated
	// routes: a stopped server, a log it has not read, a session whose evidence broke.
	// A public page that renders null as 0 tells strangers the server is empty when the
	// panel merely stopped looking.
	Players    *int       `json:"players"`
	ObservedAt *time.Time `json:"observed_at"`
	// Notice and ConnectInfo are the administrator's own plain text.
	Notice      string `json:"notice,omitempty"`
	ConnectInfo string `json:"connect_info,omitempty"`
	// Activity is one word or two for panel work keeping the server down, from publicActivity.
	Activity string `json:"activity,omitempty"`
}

// publicActivities names the jobs a stranger may learn are in progress. A kind absent here
// is never disclosed.
var publicActivities = map[jobs.Kind]string{
	jobs.KindStart:        "starting",
	jobs.KindStop:         "stopping",
	jobs.KindRestart:      "restarting",
	jobs.KindGameUpdate:   "updating",
	jobs.KindRestore:      "restoring",
	jobs.KindWorldImport:  "restoring",
	jobs.KindBackup:       "backing up",
	jobs.KindModInstall:   "changing mods",
	jobs.KindModUninstall: "changing mods",
	jobs.KindModToggle:    "changing mods",
}

// publicActivity describes what the panel is doing to a server that is not running: the job
// holding its lock when that kind is public, else a starting or stopping state, else nothing.
func publicActivity(state, jobKind string) string {
	if instance.State(state) == instance.StateRunning {
		return ""
	}
	if kind, ok := jobs.ByName(jobKind); ok {
		if activity, ok := publicActivities[kind]; ok {
			return activity
		}
	}
	if s := instance.State(state); s == instance.StateStarting || s == instance.StateStopping {
		return state
	}
	return ""
}

func (h *PublicStatus) status(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	st, err := h.DB.PublishedInstanceStatus(r.Context(), id, jobs.InstanceLockKey(id))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if st == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	view := publicStatusView{
		Name:        st.ServerName,
		Online:      st.State == string(instance.StateRunning),
		Notice:      st.Notice,
		ConnectInfo: st.ConnectInfo,
		Activity:    publicActivity(st.State, st.JobKind),
	}
	if sampler := h.Streams.Sampler(id); sampler != nil {
		if latest, ok := sampler.Latest(); ok {
			view.Players, view.ObservedAt = latest.Players, &latest.TS
		}
	}
	JSON(w, r, http.StatusOK, view)
}
