package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

const seededConfigFile = "com.example.mod.cfg"

const seededConfig = `## Settings file was created by plugin Example v1.2.0
## Plugin GUID: com.example.mod

[General]

## Whether the mod is active.
# Setting type: Boolean
# Default value: true
Enabled = true

## Damage scaling applied to enemies.
# Setting type: Single
# Default value: 1
# Acceptable value range: From 0 to 10
DamageMultiplier = 1.5
`

// seedConfigFile writes the fixture config into the instance's BepInEx config directory and
// returns its path on disk.
func seedConfigFile(t *testing.T, rt *Router) string {
	t.Helper()
	dir := filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot,
		"instances", seededInstanceID, "server", "BepInEx", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, seededConfigFile)
	if err := os.WriteFile(path, []byte(seededConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func configURL(suffix string) string {
	return "/api/v1/instances/" + seededInstanceID + "/configs" + suffix
}

// stranger is a user with no grant on inst-a, for asserting D2's 404.
func stranger(t *testing.T, db *store.DB) *store.User {
	t.Helper()
	seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-stranger', 'sam', 'argon2id$stub', 'member', ?)`, store.Now())
	return &store.User{ID: "u-stranger", Username: "sam", Role: store.RoleMember}
}

// TestConfigReadDoesNotGrantRaw asserts the escape hatch has its own capability: raw text
// bypasses every type and range the schema enforces, so config.read alone must not reach it.
func TestConfigReadDoesNotGrantRaw(t *testing.T) {
	rt, db, fake, _, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigFile(t, rt)

	typed := as(rt, member, httptest.NewRequest(http.MethodGet, configURL("/"+seededConfigFile), http.NoBody))
	if typed.Code != http.StatusOK {
		t.Errorf("typed read = %d, want 200 — viewer holds config.read (%s)", typed.Code, typed.Body)
	}
	raw := as(rt, member, httptest.NewRequest(http.MethodGet, configURL("/"+seededConfigFile+"/raw"), http.NoBody))
	if raw.Code != http.StatusForbidden {
		t.Errorf("raw read = %d, want 403 — viewer does not hold config.raw", raw.Code)
	}
}

// TestConfigsAreInvisibleWithoutInstanceView asserts D2: an instance the caller cannot see
// returns 404 on every config route, never 403, which would confirm it exists.
func TestConfigsAreInvisibleWithoutInstanceView(t *testing.T) {
	rt, db, fake, _, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigFile(t, rt)
	sam := stranger(t, db)

	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, configURL("")},
		{http.MethodGet, configURL("/" + seededConfigFile)},
		{http.MethodPatch, configURL("/" + seededConfigFile)},
		{http.MethodGet, configURL("/" + seededConfigFile + "/raw")},
		{http.MethodPut, configURL("/" + seededConfigFile + "/raw")},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := as(rt, sam, httptest.NewRequest(tt.method, tt.path, jsonBody(t, map[string]any{})))
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (D2)", rec.Code)
			}
		})
	}
}

// TestConfigFileNameCannotEscapeTheDirectory asserts B5: {file} is user input, and a name
// that resolves outside the config directory is refused rather than read.
func TestConfigFileNameCannotEscapeTheDirectory(t *testing.T) {
	inst := &store.Instance{DataDir: "/srv/valmin/instances/inst-a"}
	for _, name := range []string{
		"../../../etc/passwd",
		"../BepInEx.cfg",
		"..",
		".",
		"",
		"/etc/passwd.cfg",
		"sub/dir.cfg",
		`sub\dir.cfg`,
		"doorstop_config.ini",
		"BepInEx.cfg.bak",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := configPath(inst, name); err == nil {
				t.Errorf("configPath(%q) = %q, want an error", name, got)
			}
		})
	}
	if _, err := configPath(inst, "com.example.mod.cfg"); err != nil {
		t.Errorf("a plain .cfg name was refused: %v", err)
	}
}

// TestTraversalThroughTheRouteIsNotFound asserts an escaping name reaching the handler is a
// 404 that reads nothing, rather than an error naming what it refused (D13).
func TestTraversalThroughTheRouteIsNotFound(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigFile(t, rt)

	canary := filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot,
		"instances", seededInstanceID, "server", "BepInEx", "secret.cfg")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"..%2Fsecret.cfg", "..%2F..%2Fserver.cfg", "%2Fetc%2Fpasswd.cfg"} {
		t.Run(name, func(t *testing.T) {
			rec := as(rt, admin, httptest.NewRequest(http.MethodGet, configURL("/"+name), http.NoBody))
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
	if got, _ := os.ReadFile(canary); string(got) != "untouched" {
		t.Errorf("the file outside the config directory was modified: %q", got)
	}
}

// TestPatchConfigBacksUpAndFlagsARestart asserts the write path: the previous bytes land in
// <file>.bak exactly, one line changes, and the instance is marked as needing a restart.
func TestPatchConfigBacksUpAndFlagsARestart(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := seedConfigFile(t, rt)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, configURL("/"+seededConfigFile),
		jsonBody(t, map[string]any{"General.DamageMultiplier": 2.5})))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	after := readFile(t, path)
	if !strings.Contains(after, "DamageMultiplier = 2.5") {
		t.Errorf("the edit did not reach the file:\n%s", after)
	}
	if !strings.Contains(after, "## Damage scaling applied to enemies.") {
		t.Error("the comments did not survive the write (B10)")
	}
	if got := readFile(t, path+".bak"); got != seededConfig {
		t.Errorf(".bak does not hold the previous bytes exactly:\n%s", got)
	}

	var restart bool
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT restart_required FROM instances WHERE id = ?`, seededInstanceID).Scan(&restart); err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Error("restart_required was not set; the running server still has the old settings")
	}
}

// TestTheTwoCopiesAnswerDifferentQuestions asserts the two copies answer different
// questions: .bak follows the last write, .orig stays where the panel found the file.
func TestTheTwoCopiesAnswerDifferentQuestions(t *testing.T) {
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := seedConfigFile(t, rt)
	originalURL := configURL("/" + seededConfigFile + "/original")

	before := as(rt, admin, httptest.NewRequest(http.MethodGet, originalURL, http.NoBody))
	if before.Code != http.StatusNotFound {
		t.Errorf("before any write = %d, want 404 — nothing has been replaced yet", before.Code)
	}

	for _, value := range []float64{2.5, 3.5} {
		rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, configURL("/"+seededConfigFile),
			jsonBody(t, map[string]any{"General.DamageMultiplier": value})))
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %v = %d, want 200 (%s)", value, rec.Code, rec.Body)
		}
	}

	if got := readFile(t, path+".orig"); got != seededConfig {
		t.Errorf(".orig moved with the second write; it must hold the file as first found:\n%s", got)
	}
	if got := readFile(t, path+".bak"); !strings.Contains(got, "DamageMultiplier = 2.5") {
		t.Error(".bak does not hold the bytes the last write replaced (03 §9 rule 5)")
	}

	// The two reference points must not converge: after two edits /original is the file as
	// found and /previous is what the last write replaced.
	if got := copyValue(t, rt, admin, "/original"); got != 1.5 {
		t.Errorf("original DamageMultiplier = %v, want the pre-edit 1.5", got)
	}
	if got := copyValue(t, rt, admin, "/previous"); got != 2.5 {
		t.Errorf("previous DamageMultiplier = %v, want the first edit's 2.5", got)
	}

	// Gated on config.read, not config.raw: the same projection of the same file.
	rec := as(rt, member, httptest.NewRequest(http.MethodGet, originalURL, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("original read = %d, want 200 — viewer holds config.read (%s)", rec.Code, rec.Body)
	}
	if decodeCopy(t, rec).CapturedAt.IsZero() {
		t.Error("captured_at is empty, so the screen cannot say how old a copy is")
	}
}

// configCopy mirrors the fields of a copy response this package asserts on.
type configCopy struct {
	CapturedAt time.Time `json:"captured_at"`
	Sections   []struct {
		Settings []struct {
			Key     string `json:"key"`
			Current any    `json:"current"`
		} `json:"settings"`
	} `json:"sections"`
}

func decodeCopy(t *testing.T, rec *httptest.ResponseRecorder) configCopy {
	t.Helper()
	var view configCopy
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// copyValue reads DamageMultiplier out of one of the copy endpoints.
func copyValue(t *testing.T, rt *Router, u *store.User, suffix string) any {
	t.Helper()
	url := configURL("/" + seededConfigFile + suffix)
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, url, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d, want 200 (%s)", suffix, rec.Code, rec.Body)
	}
	for _, section := range decodeCopy(t, rec).Sections {
		for _, setting := range section.Settings {
			if setting.Key == "DamageMultiplier" {
				return setting.Current
			}
		}
	}
	t.Fatalf("%s served no DamageMultiplier", suffix)
	return nil
}

// TestPatchConfigOnARunningInstanceIsRefused asserts ADR-012's gate, and that the refusal
// happens before the file is touched.
func TestPatchConfigOnARunningInstanceIsRefused(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, instanceStateRunning)
	path := seedConfigFile(t, rt)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, configURL("/"+seededConfigFile),
		jsonBody(t, map[string]any{"General.Enabled": false})))
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := readFile(t, path); got != seededConfig {
		t.Error("the file was modified despite the refusal")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a .bak was written for an edit that was refused")
	}
}

// TestPatchConfigReportsEveryProblemAndWritesNothing asserts 11 §2.4: one request, one
// response, all the problems — and a patch mixing valid and invalid keys writes neither.
func TestPatchConfigReportsEveryProblemAndWritesNothing(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := seedConfigFile(t, rt)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, configURL("/"+seededConfigFile),
		jsonBody(t, map[string]any{
			"General.Enabled":          false,
			"General.DamageMultiplier": 99.0,
			"General.Missing":          "x",
		})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%s)", rec.Code, rec.Body)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Fields []struct {
					Field string `json:"field"`
					Code  string `json:"code"`
				} `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &body)
	if body.Error.Code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", body.Error.Code)
	}
	if n := len(body.Error.Details.Fields); n != 2 {
		t.Fatalf("got %d field errors, want 2: %+v", n, body.Error.Details.Fields)
	}
	want := map[string]string{
		"General.DamageMultiplier": "out_of_range",
		"General.Missing":          "unknown_setting",
	}
	for _, f := range body.Error.Details.Fields {
		if want[f.Field] != f.Code {
			t.Errorf("%s: code = %q, want %q", f.Field, f.Code, want[f.Field])
		}
	}
	if got := readFile(t, path); got != seededConfig {
		t.Error("the valid field was written even though the patch failed")
	}
}

// TestRawPutNeedsAFreshETag asserts G1: a full replacement without If-Match is a client bug
// (400), and with a stale one is a real concurrent edit (412 stale_write).
func TestRawPutNeedsAFreshETag(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := seedConfigFile(t, rt)
	rawURL := configURL("/" + seededConfigFile + "/raw")

	get := as(rt, admin, httptest.NewRequest(http.MethodGet, rawURL, http.NoBody))
	if get.Code != http.StatusOK {
		t.Fatalf("raw get = %d, want 200", get.Code)
	}
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the raw read served no ETag, so a client cannot write safely")
	}

	edited := strings.Replace(seededConfig, "Enabled = true", "Enabled = false", 1)

	missing := as(rt, admin, rawPut(rawURL, edited, ""))
	if missing.Code != http.StatusBadRequest {
		t.Errorf("without If-Match = %d, want 400 (%s)", missing.Code, missing.Body)
	}

	stale := rawPut(rawURL, edited, `"0000000000000000000000000000000000000000000000000000000000000000"`)
	if rec := as(rt, admin, stale); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("with a stale If-Match = %d, want 412 (%s)", rec.Code, rec.Body)
	}
	if got := readFile(t, path); got != seededConfig {
		t.Fatal("a refused write reached the file")
	}

	if rec := as(rt, admin, rawPut(rawURL, edited, etag)); rec.Code != http.StatusOK {
		t.Fatalf("with a fresh If-Match = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := readFile(t, path); got != edited {
		t.Error("the accepted write did not reach the file")
	}
	if got := readFile(t, path+".bak"); got != seededConfig {
		t.Error("the raw path did not write a .bak of the previous bytes")
	}
}

// TestConfigListEmptyStateSpeaksForItself asserts ADR-110: the daemon composes the sentence,
// because the SPA holds no Valheim knowledge to compose it with (F2).
func TestConfigListEmptyStateSpeaksForItself(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, configURL(""), http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body configListView
	decodeInto(t, rec, &body)
	if len(body.Items) != 0 {
		t.Errorf("items = %+v, want none", body.Items)
	}
	if body.Note == "" {
		t.Error("no note; the screen has nothing to render but a blank page")
	}
}

// TestConfigListNamesThePluginBehindEachFile asserts the list carries the plugin name from
// each file's own header, which is the only link a `.cfg` holds.
func TestConfigListNamesThePluginBehindEachFile(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seedConfigFile(t, rt)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, configURL(""), http.NoBody))
	var body configListView
	decodeInto(t, rec, &body)
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v, want 1", body.Items)
	}
	if body.Items[0].File != seededConfigFile || body.Items[0].Plugin != "Example 1.2.0" {
		t.Errorf("item = %+v", body.Items[0])
	}
	if body.Note != "" {
		t.Errorf("note = %q on a list that has files", body.Note)
	}
}

// rawPut builds a raw config write: the body is the file itself, as 04 §3 specifies.
func rawPut(url, content, ifMatch string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, url, strings.NewReader(content))
	r.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	return r
}

// TestRawPutRefusesAJSONBody asserts the escape hatch takes the file, not an envelope: a
// JSON body would be written into the config verbatim.
func TestRawPutRefusesAJSONBody(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	path := seedConfigFile(t, rt)

	req := httptest.NewRequest(http.MethodPut, configURL("/"+seededConfigFile+"/raw"),
		jsonBody(t, map[string]string{"content": "x"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"whatever"`)
	if rec := as(rt, admin, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 (%s)", rec.Code, rec.Body)
	}
	if got := readFile(t, path); got != seededConfig {
		t.Error("the refused body reached the file")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
