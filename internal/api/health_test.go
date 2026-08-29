package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

func health(t *testing.T) (h *Health, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.db")
	db, err := store.Open(t.Context(), "sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &Health{DB: db, Runtime: runtime.NewFake()}, path
}

func probe(t *testing.T, h *Health, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

func components(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body struct {
		Ready      bool              `json:"ready"`
		Components map[string]string `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body.Components
}

// A liveness probe that fails because the database is gone restarts a panel that is
// perfectly alive, and every restart means the lease dance of 12 §5.1 for no reason.
func TestHealthzAnswersWithTheDatabaseDeletedUnderneathIt(t *testing.T) {
	h, path := health(t)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the database: %v", err)
	}

	if rec := probe(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d with the database deleted, want 200", rec.Code)
	}
}

func TestReadyzPassesWhenBothComponentsAnswer(t *testing.T) {
	h, _ := health(t)

	rec := probe(t, h, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d (%s), want 200", rec.Code, rec.Body)
	}
	if got := components(t, rec); len(got) != 0 {
		t.Errorf("components = %v, want none", got)
	}
}

func TestReadyzNamesTheFailingComponent(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(*Health, string)
		want    string
	}{
		{
			name:    "docker",
			breakIt: func(h *Health, _ string) { h.Runtime.(*runtime.Fake).PingErr = errors.New("socket not mounted") },
			want:    "docker",
		},
		{
			name:    "database",
			breakIt: func(h *Health, _ string) { _ = h.DB.Close() },
			want:    "database",
		},
		{
			name:    "draining",
			breakIt: func(h *Health, _ string) { h.Drain() },
			want:    "server",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, path := health(t)
			tc.breakIt(h, path)

			rec := probe(t, h, "/readyz")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("/readyz = %d, want 503", rec.Code)
			}
			got := components(t, rec)
			if _, ok := got[tc.want]; !ok {
				t.Errorf("components = %v, want a %q entry: a 503 that does not say which "+
					"component failed makes an operator guess (11 §10)", got, tc.want)
			}
		})
	}
}

// The probe's database question has to fail on a schema that is not there, not only on a
// socket that is not answering: 11 §10 names "migrations are applied" as its own component.
func TestReadyzFailsOnAnUnmigratedDatabase(t *testing.T) {
	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &Health{DB: db, Runtime: runtime.NewFake()}

	if rec := probe(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d on an unmigrated database, want 503", rec.Code)
	}
}
