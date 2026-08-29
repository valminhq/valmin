package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Health serves the two probe endpoints of 11 §10. They sit outside the middleware chain
// and outside the error envelope: a probe is not an API client.
type Health struct {
	DB      *store.DB
	Runtime runtime.Runtime

	draining atomic.Bool
}

// Routes registers the probes on mux.
func (h *Health) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.live)
	mux.HandleFunc("GET /readyz", h.ready)
}

// Drain makes /readyz report unready, so a proxy stops sending traffic before the server
// stops accepting it (11 §10 step 1).
func (h *Health) Drain() { h.draining.Store(true) }

// live answers as long as the process runs. It must not touch the database: a liveness
// probe that fails because SQLite is busy restarts a panel that was merely under load,
// and restarting means the daemon lease dance of 12 §5.1 for no reason.
func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ready reports whether the panel can serve work, naming the component that cannot.
func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	components := map[string]string{}
	if h.draining.Load() {
		components["server"] = "draining"
	}
	// One round trip answers both "reachable" and "migrated": counting the applied rows
	// needs the table the migrations create.
	if err := h.DB.MigrationsApplied(r.Context()); err != nil {
		components["database"] = err.Error()
	}
	if err := h.Runtime.Ping(r.Context()); err != nil {
		components["docker"] = err.Error()
	}

	status := http.StatusOK
	if len(components) > 0 {
		status = http.StatusServiceUnavailable
		slog.WarnContext(r.Context(), "readiness probe failed", slog.Any("components", components))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":      status == http.StatusOK,
		"components": components,
	})
}
