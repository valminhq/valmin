package api

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

func statusPath(id string) string { return "/public/status/" + id }

// public sends a request the way a stranger would: no session, no CSRF token, no Origin.
func public(rt *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

// TestANewInstanceIsPrivate is the default that makes this feature safe to ship at all: a
// panel that published on creation would put every server's name on the internet until
// somebody noticed (05 M5).
func TestANewInstanceIsPrivate(t *testing.T) {
	rt, _, _, _ := world(t)
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestUnpublishedAndMissingAreTheSameAnswer. On an internet-facing route a distinguishable
// "exists but private" is an existence oracle for every world on the panel (ADR-038, D2).
func TestUnpublishedAndMissingAreTheSameAnswer(t *testing.T) {
	rt, _, _, _ := world(t)

	private := public(rt, statusPath("inst-a"))
	missing := public(rt, statusPath("no-such-instance"))
	if private.Code != missing.Code {
		t.Fatalf("private = %d, missing = %d; they must be indistinguishable", private.Code, missing.Code)
	}
	// Bodies differ only in the request id every response carries.
	var privateErr, missingErr struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	decodeInto(t, private, &privateErr)
	decodeInto(t, missing, &missingErr)
	if privateErr.Error != missingErr.Error {
		t.Errorf("private %+v differs from missing %+v", privateErr.Error, missingErr.Error)
	}
}

// TestPublishingIsAnAdminDecision. The column is the authorization for the public route, so
// who may set it is the whole access-control story (ADR-156).
func TestPublishingIsAnAdminDecision(t *testing.T) {
	rt, db, admin, member := world(t)

	memberRec := as(rt, member, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true})))
	if memberRec.Code != http.StatusForbidden {
		t.Fatalf("member published = %d, want 403 (%s)", memberRec.Code, memberRec.Body)
	}
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Fatalf("refused publish still exposed the instance: %d", rec.Code)
	}

	adminRec := as(rt, admin, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true})))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin publish = %d, want 200 (%s)", adminRec.Code, adminRec.Body)
	}
	var updated struct {
		StatusPublished bool `json:"status_published"`
		RestartRequired bool `json:"restart_required"`
	}
	decodeInto(t, adminRec, &updated)
	if !updated.StatusPublished {
		t.Error("status_published did not stick")
	}
	// It shapes no container, so telling the operator to restart for it would be false.
	if updated.RestartRequired {
		t.Error("publishing set restart_required")
	}

	rec := public(rt, statusPath("inst-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("published instance = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE action = 'instances.status.published'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("audit rows for the publication = %d, want 1", count)
	}
}

// TestUnpublishingTakesItBack. The toggle has to work in both directions or the operator
// cannot undo a mistake.
func TestUnpublishingTakesItBack(t *testing.T) {
	rt, _, admin, _ := world(t)
	for _, on := range []bool{true, false} {
		rec := as(rt, admin, httptest.NewRequest(
			http.MethodPatch, "/api/v1/instances/inst-a",
			jsonBody(t, map[string]any{"status_published": on})))
		if rec.Code != http.StatusOK {
			t.Fatalf("set published=%v = %d (%s)", on, rec.Code, rec.Body)
		}
	}
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished instance still readable: %d (%s)", rec.Code, rec.Body)
	}
}

// TestThePublicPayloadCarriesNothingElse pins the contract. Every field on this route is a
// deliberate disclosure to strangers, so the test names what must never appear rather than
// only what must.
func TestThePublicPayloadCarriesNothingElse(t *testing.T) {
	rt, _, admin, _ := world(t)
	if rec := as(rt, admin, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true}))); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body)
	}

	rec := public(rt, statusPath("inst-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Name    string `json:"name"`
		Online  bool   `json:"online"`
		Players *int   `json:"players"`
	}
	decodeInto(t, rec, &got)
	if got.Name != "Server inst-a" {
		t.Errorf("name = %q, want the server name", got.Name)
	}
	if got.Online {
		t.Error("a stopped instance reported online")
	}
	// Never zero for a server nobody is reading the log of: that is E7's lie, and on a public
	// page it tells strangers the server is empty.
	if got.Players != nil {
		t.Errorf("players = %d, want null", *got.Players)
	}

	for _, leak := range []string{
		"data_dir", "/srv/valmin", "password", "base_port", "2456", "crossplay", "cp-inst-a",
		"world", "state", "container", "grant",
	} {
		if bytes.Contains(bytes.ToLower(rec.Body.Bytes()), []byte(leak)) {
			t.Errorf("public payload mentions %q: %s", leak, rec.Body)
		}
	}
}

// TestThePublicRouteHasItsOwnLimit. It is the only route a stranger can reach, and it must
// not spend the budget the login route shares (11 §7).
func TestThePublicRouteHasItsOwnLimit(t *testing.T) {
	rt, _, _, _ := world(t)

	var limited bool
	for range publicStatusBurst + 5 {
		if public(rt, statusPath("inst-a")).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the public route never rate limited")
	}
	// The chain-wide limiter is a different bucket, so an authenticated caller is unaffected.
	if rec := public(rt, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("the public limiter reached an unrelated route: %d", rec.Code)
	}
}

// TestThePublicRouteCarriesSecurityHeaders is what separates it from the health probes, which
// are registered on the bare mux and would be the wrong seam to reuse (11 §10).
func TestThePublicRouteCarriesSecurityHeaders(t *testing.T) {
	rt, _, _, _ := world(t)
	rec := public(rt, statusPath("inst-a"))
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("no request id; an operator cannot tie a report to a log line")
	}
}

// statusWorld is lifecycleWorld with inst-a seeded stopped, for bodies larger than world()'s
// router accepts.
func statusWorld(t *testing.T) (*Server, *store.DB, *store.User) {
	t.Helper()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	return rt, db, admin
}

// publishWith publishes inst-a as admin, with fields added to the PATCH body.
func publishWith(t *testing.T, rt *Server, admin *store.User, fields map[string]any) {
	t.Helper()
	body := map[string]any{"status_published": true}
	maps.Copy(body, fields)
	if rec := patchInstance(t, rt, admin, body); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body)
	}
}

// publicFields reads the public response as a map, so an omitted key is distinguishable from
// an empty one.
func publicFields(t *testing.T, rt *Server) map[string]any {
	t.Helper()
	rec := public(rt, statusPath("inst-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var got map[string]any
	decodeInto(t, rec, &got)
	return got
}

// TestThePublicPayloadCarriesTheStatusText asserts the notice and connection guidance are
// served trimmed with their line breaks, and omitted when empty.
func TestThePublicPayloadCarriesTheStatusText(t *testing.T) {
	tests := []struct {
		name                string
		fields              map[string]any
		notice, connectInfo any
	}{
		{"neither set", nil, nil, nil},
		{"a notice alone", map[string]any{"status_notice": "Down until 20:00"}, "Down until 20:00", nil},
		{
			"both, over several lines",
			map[string]any{
				"status_notice":       "  Updating.\nBack soon.  ",
				"status_connect_info": "Join play.example:2456\nAsk in chat for the password.",
			},
			"Updating.\nBack soon.", "Join play.example:2456\nAsk in chat for the password.",
		},
		{"whitespace only", map[string]any{"status_notice": " \n "}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, admin := statusWorld(t)
			publishWith(t, rt, admin, tt.fields)

			got := publicFields(t, rt)
			if got["notice"] != tt.notice {
				t.Errorf("notice = %#v, want %#v", got["notice"], tt.notice)
			}
			if got["connect_info"] != tt.connectInfo {
				t.Errorf("connect_info = %#v, want %#v", got["connect_info"], tt.connectInfo)
			}
		})
	}
}

// TestStatusTextIsValidated asserts each field is limited to 500 characters, counted as
// characters rather than bytes, and that a rejected body writes nothing.
func TestStatusTextIsValidated(t *testing.T) {
	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"a notice at the limit", map[string]any{"status_notice": strings.Repeat("a", 500)}, ""},
		{"multibyte text at the limit", map[string]any{"status_notice": strings.Repeat("é", 500)}, ""},
		{"a notice over the limit", map[string]any{"status_notice": strings.Repeat("a", 501)}, "status_notice"},
		{
			"guidance over the limit",
			map[string]any{"status_connect_info": strings.Repeat("a", 501)},
			"status_connect_info",
		},
		{
			"alongside a launch field",
			map[string]any{"server_name": "Renamed", "status_notice": strings.Repeat("a", 501)},
			"status_notice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin := statusWorld(t)
			rec := patchInstance(t, rt, admin, tt.body)
			if tt.field == "" {
				if rec.Code != http.StatusOK {
					t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("patch = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			var envelope struct {
				Error struct {
					Details struct {
						Fields []struct{ Field, Code string } `json:"fields"`
					} `json:"details"`
				} `json:"error"`
			}
			decodeInto(t, rec, &envelope)
			fields := envelope.Error.Details.Fields
			if len(fields) != 1 || fields[0].Field != tt.field || fields[0].Code != "invalid" {
				t.Errorf("fields = %+v, want one invalid %s", fields, tt.field)
			}
			inst, err := db.InstanceByID(t.Context(), "inst-a")
			if err != nil {
				t.Fatal(err)
			}
			if inst.StatusNotice != "" || inst.StatusConnectInfo != "" || inst.ServerName != "Server" {
				t.Errorf("a rejected patch wrote the row: %+v", inst)
			}
		})
	}
}

// TestStatusTextIsASettingsDecision asserts the text needs instance.settings, is audited under
// instances.status.update with its changes, and records nothing when it did not change.
func TestStatusTextIsASettingsDecision(t *testing.T) {
	rt, db, admin, member := world(t)

	if rec := patchInstance(t, rt, member, map[string]any{"status_notice": "hi"}); rec.Code != http.StatusForbidden {
		t.Fatalf("member notice = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	for range 2 {
		if rec := patchInstance(t, rt, admin, map[string]any{"status_notice": "hi"}); rec.Code != http.StatusOK {
			t.Fatalf("admin notice = %d (%s)", rec.Code, rec.Body)
		}
	}
	rows := auditRecordsFor(t, db, "instances.status.update")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if got, want := deref(rows[0].Detail), `{"changes":[{"field":"status_notice","from":"","to":"hi"}]}`; got != want {
		t.Errorf("detail = %s, want %s", got, want)
	}
	if n := len(auditRecordsFor(t, db, "instances.settings.update")); n != 0 {
		t.Errorf("settings audit rows = %d, want none", n)
	}
}

// TestUnpublishedTextStaysPrivate asserts an unpublished instance with text and a running job
// answers exactly as a missing one does.
func TestUnpublishedTextStaysPrivate(t *testing.T) {
	rt, db, admin := statusWorld(t)
	publishWith(t, rt, admin, map[string]any{"status_notice": "Down", "status_connect_info": "Join"})
	if rec := patchInstance(t, rt, admin, map[string]any{"status_published": false}); rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d (%s)", rec.Code, rec.Body)
	}
	seedInstanceJob(t, db, jobs.KindGameUpdate)

	private := public(rt, statusPath("inst-a"))
	missing := public(rt, statusPath("no-such-instance"))
	if private.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("private = %d, missing = %d, want 404 for both", private.Code, missing.Code)
	}
	if strings.Contains(private.Body.String(), "Down") {
		t.Errorf("unpublished body carries the notice: %s", private.Body)
	}
}

// TestStatusTextFailureKeepsThePagePrivate checks both combined publication decisions when
// storing the text fails after the request has been validated.
func TestStatusTextFailureKeepsThePagePrivate(t *testing.T) {
	for _, initiallyPublished := range []bool{false, true} {
		t.Run(fmt.Sprintf("initially published %v", initiallyPublished), func(t *testing.T) {
			rt, db, admin := statusWorld(t)
			if initiallyPublished {
				publishWith(t, rt, admin, nil)
			}
			if _, err := db.Writer.ExecContext(t.Context(), `
				CREATE TRIGGER reject_status_text BEFORE UPDATE OF status_notice ON instances
				BEGIN SELECT RAISE(ABORT, 'status text unavailable'); END`); err != nil {
				t.Fatal(err)
			}

			rec := patchInstance(t, rt, admin, map[string]any{
				"status_published": !initiallyPublished,
				"status_notice":    "Do not show this text",
			})
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("patch = %d, want 500 (%s)", rec.Code, rec.Body)
			}
			if publicRec := public(rt, statusPath("inst-a")); publicRec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (%s)", publicRec.Code, publicRec.Body)
			}
		})
	}
}

// seedInstanceJob records a running job of kind holding inst-a's lock and returns its id.
func seedInstanceJob(t *testing.T, db *store.DB, kind jobs.Kind) string {
	t.Helper()
	id := store.NewID()
	seed(t, db, `INSERT INTO job_runs (
		id, kind, status, lock_key, instance_id, instance_name, payload, created_at, started_at
	) VALUES (?, ?, 'running', ?, 'inst-a', 'inst-a', '{}', ?, ?)`,
		id, kind.String(), jobs.InstanceLockKey("inst-a"), store.Now(), store.Now())
	seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		jobs.InstanceLockKey("inst-a"), id, store.Now())
	return id
}

// TestThePublicPayloadNamesTheActivity asserts the response carries the coarse activity for
// the job holding the instance, omits it when there is none to disclose, and never carries
// the job itself.
func TestThePublicPayloadNamesTheActivity(t *testing.T) {
	tests := []struct {
		name  string
		state string
		kind  *jobs.Kind
		want  any
	}{
		{"a restart between stop and start", "stopping", &jobs.KindRestart, "restarting"},
		{"a game update", "updating", &jobs.KindGameUpdate, "updating"},
		{"a mod install", "stopped", &jobs.KindModInstall, "changing mods"},
		{"a start with no job", "starting", nil, "starting"},
		{"an idle stopped server", "stopped", nil, nil},
		{"a hot backup of a running server", "running", &jobs.KindBackup, nil},
		{"an undisclosed kind", "stopped", &jobs.KindClone, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, _ := world(t)
			publishWith(t, rt, admin, nil)
			seed(t, db, `UPDATE instances SET state = ? WHERE id = 'inst-a'`, tt.state)
			jobID := ""
			if tt.kind != nil {
				jobID = seedInstanceJob(t, db, *tt.kind)
			}

			rec := public(rt, statusPath("inst-a"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			var got map[string]any
			decodeInto(t, rec, &got)
			if got["activity"] != tt.want {
				t.Errorf("activity = %#v, want %#v", got["activity"], tt.want)
			}
			if jobID != "" && strings.Contains(rec.Body.String(), jobID) {
				t.Errorf("public payload carries the job id: %s", rec.Body)
			}
		})
	}
}

// TestPublicActivity asserts the mapping from instance state and lock-holding job kind to the
// one public activity word.
func TestPublicActivity(t *testing.T) {
	tests := []struct {
		state, kind, want string
	}{
		{"stopped", "", ""},
		{"running", "", ""},
		{"running", "backup", ""},
		{"starting", "", "starting"},
		{"stopping", "", "stopping"},
		{"starting", "restart", "restarting"},
		{"stopping", "backup", "backing up"},
		{"updating", "game_update", "updating"},
		{"restoring", "restore", "restoring"},
		{"stopped", "world_import", "restoring"},
		{"stopped", "mod_install", "changing mods"},
		{"stopped", "mod_uninstall", "changing mods"},
		{"stopped", "mod_toggle", "changing mods"},
		{"stopped", "clone", ""},
		{"stopped", "no_such_kind", ""},
		{"starting", "clone", "starting"},
	}
	for _, tt := range tests {
		if got := publicActivity(tt.state, tt.kind); got != tt.want {
			t.Errorf("publicActivity(%q, %q) = %q, want %q", tt.state, tt.kind, got, tt.want)
		}
	}
}
