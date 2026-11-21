package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
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
	// Modpacks reads a server's stored profile code while its mods still match it.
	Modpacks modpackReader
}

// modpackReader is the stored profile code lookup the status route needs.
type modpackReader interface {
	currentModpackCode(ctx context.Context, instanceID string) (*store.ModpackCode, error)
}

// publicStatusRoutes registers the route on mux behind its own thin chain, outside the API chain and
// outside the API subtree. It is not registered on the bare mux the health probes use: 11 §10
// exempts those from rate limiting and security headers, which is fine for a liveness probe on
// a LAN and not for an internet-facing route.
func publicStatusRoutes(mux *http.ServeMux, chain []middleware.Layer, h *PublicStatus) {
	mux.Handle("GET /public/status/{id}", middleware.Apply(http.HandlerFunc(h.status), chain))
}

// publicStatusView is the whole public contract, and it is short on purpose: the page answers
// "is it up", "who is on", what the administrator chose to say, and how to join only when the
// administrator turned that on. No resource usage, no world name, no state beyond up, down, and
// a coarse activity. Every field here is a disclosure to strangers.
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
	// JoinAddress and SteamJoinURL, or JoinCode with crossplay, are set when the page shows how
	// to join. ModpackCode and ModpackRegistry are set when it shows the profile code and the
	// stored one still matches the server's mods.
	JoinAddress     string `json:"join_address,omitempty"`
	SteamJoinURL    string `json:"steam_join_url,omitempty"`
	JoinCode        string `json:"join_code,omitempty"`
	ModpackCode     string `json:"modpack_code,omitempty"`
	ModpackRegistry string `json:"modpack_registry,omitempty"`
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if st == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}

	view := publicStatusView{
		Name:        st.ServerName,
		Online:      st.State == string(instance.StateRunning),
		Notice:      st.Notice,
		ConnectInfo: st.ConnectInfo,
		Activity:    publicActivity(st.State, st.JobKind),
	}
	h.addPlayers(&view, id)
	if st.Show.Join {
		h.addJoin(&view, id, st)
	}
	if st.Show.Modpack {
		code, err := h.Modpacks.currentModpackCode(r.Context(), id)
		if err != nil {
			slog.WarnContext(r.Context(), "could not read the profile code for the status page",
				slog.String("instance_id", id), slog.Any("error", err))
		}
		if code != nil {
			view.ModpackCode, view.ModpackRegistry = code.Code, code.Registry
		}
	}
	JSON(w, r, http.StatusOK, view)
}

// addPlayers adds the log reader's current player count while the server's container is
// sampled, which it is not once the server stops.
func (h *PublicStatus) addPlayers(view *publicStatusView, id string) {
	sampler, reader := h.Streams.Sampler(id), h.Streams.Reader(id)
	if sampler == nil || reader == nil {
		return
	}
	if _, ok := sampler.Latest(); !ok {
		return
	}
	now := time.Now().UTC()
	view.Players, view.ObservedAt = reader.Players(), &now
}

// addJoin fills in how to join: the join code this boot logged on a crossplay server, else the
// join address and its Steam link when a join host is set.
func (h *PublicStatus) addJoin(view *publicStatusView, id string, st *store.PublishedStatus) {
	if st.Crossplay {
		if reader := h.Streams.Reader(id); reader != nil {
			view.JoinCode = reader.JoinCode()
		}
		return
	}
	addr := joinAddress(st.JoinHost, st.BasePort)
	if addr == nil {
		return
	}
	view.JoinAddress = *addr
	if link := steamJoinURL(addr, false); link != nil {
		view.SteamJoinURL = *link
	}
}
