package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/backup/rclonesetup"
	"github.com/valminhq/valmin/internal/backup/remotecopy"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const s3SetupBody = `{"provider":"s3","name":"s3-backups","s3_provider":"AWS",` +
	`"access_key_id":"AKIAFIXTURE","secret_access_key":"secret-key-fixture"}`

type stubRclone struct{}

func (stubRclone) PrepareConfig() error                            { return nil }
func (stubRclone) Remotes(context.Context) ([]string, error)       { return nil, nil }
func (stubRclone) Obscure(context.Context, string) (string, error) { return "obscured", nil }

// rcloneSetupWorld points the panel at a private, not yet created rclone configuration.
func rcloneSetupWorld(t *testing.T) (rt *Server, db *store.DB, admin, member *store.User, config string) {
	t.Helper()
	rt, db, _, admin, member = lifecycleWorld(t)
	config = filepath.Join(t.TempDir(), "rclone", "rclone.conf")
	rt.remoteBackups.Cfg.RemoteBackups.RcloneConfig = config
	rt.remoteBackups.Setups = rclonesetup.New(stubRclone{}, http.DefaultClient, nil)
	return rt, db, admin, member, config
}

// inSession sends r as u within a login session, with the CSRF token the SPA would echo.
func inSession(t *testing.T, rt *Server, u *store.User, session string, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	token, err := middleware.CSRFToken(rt.remoteBackups.Keeper, session)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-CSRF-Token", token)
	return as(rt, u, r.WithContext(middleware.WithSessionID(r.Context(), session)))
}

func startS3Setup(t *testing.T, rt *Server, admin *store.User) rclonesetup.Status {
	t.Helper()
	rec := inSession(t, rt, admin, "session-a", jsonReq(http.MethodPost, "/api/v1/admin/rclone-setups", s3SetupBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start setup = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secret-key-fixture") {
		t.Fatalf("setup response exposes the secret key: %s", rec.Body)
	}
	var status rclonesetup.Status
	decodeInto(t, rec, &status)
	return status
}

func jsonReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestRcloneSetupWritesTheRemoteThroughAJob(t *testing.T) {
	rt, db, admin, member, config := rcloneSetupWorld(t)
	if rec := inSession(
		t,
		rt,
		member,
		"session-m",
		jsonReq(http.MethodPost, "/api/v1/admin/rclone-setups", s3SetupBody),
	); rec.Code != http.StatusNotFound {
		t.Fatalf("member start = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	status := startS3Setup(t, rt, admin)
	setupPath := "/api/v1/admin/rclone-setups/" + status.ID
	if rec := inSession(
		t,
		rt,
		admin,
		"session-b",
		httptest.NewRequest(http.MethodGet, setupPath, http.NoBody),
	); rec.Code != http.StatusNotFound {
		t.Fatalf("setup read from another login session = %d, want 404", rec.Code)
	}

	rec := inSession(t, rt, admin, "session-a", httptest.NewRequest(http.MethodPost, setupPath+"/finish", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	decodeInto(t, rec, &accepted)
	if job := waitJob(t, rt, admin, accepted.JobID); job.Status != jobs.StatusSucceeded {
		t.Fatalf("remote_create job = %+v, want success", job)
	}

	contents, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "[s3-backups]\ntype = s3\n") ||
		!strings.Contains(string(contents), "secret_access_key = secret-key-fixture\n") {
		t.Fatalf("rclone.conf = %q", contents)
	}
	var payload, detail string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT payload FROM job_runs WHERE id = ?`, accepted.JobID).
		Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT detail FROM audit_log WHERE action = 'remote_backups.connection.create'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, recorded := range []string{payload, detail} {
		if strings.Contains(recorded, "secret-key-fixture") || strings.Contains(recorded, "AKIAFIXTURE") ||
			!strings.Contains(recorded, "s3-backups") {
			t.Errorf("recorded %q; want the name and provider without credentials", recorded)
		}
	}
	if rec := inSession(
		t,
		rt,
		admin,
		"session-a",
		httptest.NewRequest(http.MethodGet, setupPath, http.NoBody),
	); rec.Code != http.StatusNotFound {
		t.Fatalf("setup read after saving = %d, want 404", rec.Code)
	}
}

func TestRcloneSetupFinishWaitsForTheRemoteBackupLock(t *testing.T) {
	rt, _, admin, _, _ := rcloneSetupWorld(t)
	release := make(chan struct{})
	holder, err := rt.remoteBackups.Engine.Submit(t.Context(), &jobs.Spec{
		Kind: jobs.KindRemoteTest, LockKey: remotecopy.LockKey,
	}, func(context.Context, *jobs.Handle) jobs.Outcome {
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}
	status := startS3Setup(t, rt, admin)
	setupPath := "/api/v1/admin/rclone-setups/" + status.ID
	rec := inSession(t, rt, admin, "session-a", httptest.NewRequest(http.MethodPost, setupPath+"/finish", http.NoBody))
	close(release)
	waitJob(t, rt, admin, holder.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "job_in_progress") {
		t.Fatalf("finish while a transfer holds the lock = %d (%s), want 409 job_in_progress", rec.Code, rec.Body)
	}
	if rec := inSession(
		t,
		rt,
		admin,
		"session-a",
		httptest.NewRequest(http.MethodGet, setupPath, http.NoBody),
	); rec.Code != http.StatusOK {
		t.Fatalf("setup read after a refused finish = %d, want 200 so it can be retried", rec.Code)
	}
}

func TestRcloneSetupReportsFieldErrors(t *testing.T) {
	rt, _, admin, _, _ := rcloneSetupWorld(t)
	rec := inSession(t, rt, admin, "session-a", jsonReq(http.MethodPost, "/api/v1/admin/rclone-setups",
		`{"provider":"drive","name":"drive-backups","client_id":"c"}`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"field":"client_secret"`) {
		t.Fatalf("start without a client secret = %d (%s), want 422 naming client_secret", rec.Code, rec.Body)
	}
}
