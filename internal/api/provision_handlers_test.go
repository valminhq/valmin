package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// provisionWorld is world()'s shape, but with a real, writable Data.Root/HostRoot: the
// provision job's Runner touches the filesystem the moment Submit returns (it runs in its
// own goroutine), and world()'s router points Data.Root at /srv/valmin, which does not
// exist in a test environment.
func provisionWorld(t *testing.T) (rt *Router, db *store.DB, admin, member *store.User) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)

	rt, err = NewRouter(&cfg, h.DB, h, k, false, testEngine(h.DB, &cfg), runtime.NewFake())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, u := range []struct {
		id, name string
		role     store.Role
	}{{"u-admin", "ada", store.RoleAdmin}, {"u-member", "mel", store.RoleMember}} {
		seed(t, h.DB, `INSERT INTO users (id, username, password_hash, role, created_at)
			VALUES (?, ?, 'argon2id$stub', ?, ?)`, u.id, u.name, string(u.role), store.Now())
	}

	return rt, h.DB,
		&store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin},
		&store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}
}

func validCreateBody(name string) map[string]any {
	return map[string]any{
		"name": name, "server_name": "My Server", "world_name": "MyWorld", "password": "hunter2",
	}
}

// TestCreateInstanceRequiresAdmin is 09 §3.3: instance.create is never grantable, so a
// member is rejected before any validation runs.
func TestCreateInstanceRequiresAdmin(t *testing.T) {
	rt, _, _, member := provisionWorld(t)
	rec := as(rt, member, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("member-cannot"))))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestCreateInstanceValidatesLaunchConfig is 03 §1.3's three rules, checked at this
// boundary in addition to instance.BuildSpec's own re-check (G2).
func TestCreateInstanceValidatesLaunchConfig(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	body := map[string]any{
		"name": "bad-config", "server_name": "same", "world_name": "same", "password": "abc",
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}

	var got struct {
		Error struct {
			Details struct {
				Fields []struct {
					Field string `json:"field"`
					Code  string `json:"code"`
				} `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &got)
	if len(got.Error.Details.Fields) < 2 {
		t.Errorf("fields = %+v, want at least password_too_short and world_same_as_server",
			got.Error.Details.Fields)
	}
}

// TestCreateInstanceRequiresAName is the one field 03 §1.3 does not already cover: the
// panel's own unique label, distinct from the in-game server_name.
func TestCreateInstanceRequiresAName(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	body := validCreateBody("")
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// TestCreateInstanceRejectsDuplicateName proves store.ErrInstanceNameTaken reaches the
// wire as 409 name_taken, distinct from a port collision.
func TestCreateInstanceRejectsDuplicateName(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)

	first := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("dupe-name"))))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want 202 (%s)", first.Code, first.Body)
	}

	second := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("dupe-name"))))
	if second.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409 (%s)", second.Code, second.Body)
	}
	if got := errCode(t, second); got != "name_taken" {
		t.Errorf("code = %q, want name_taken", got)
	}
}

// TestCreateInstanceReturns202AndMovesInstanceToProvisioning is 11 §3's contract end to
// end: the response is a job, never the instance, and by the time it is written the
// instance row already exists and has already been claimed into `provisioning` — both
// happen synchronously before Submit returns (12 §6's Claim phase).
func TestCreateInstanceReturns202AndMovesInstanceToProvisioning(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("fresh-instance"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want /api/v1/jobs/...", loc)
	}

	var stub struct {
		JobID  string `json:"job_id"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	decodeInto(t, rec, &stub)
	if stub.JobID == "" {
		t.Error("job_id is empty")
	}
	if stub.Kind != "provision" {
		t.Errorf("kind = %q, want provision", stub.Kind)
	}
	if stub.Status != "running" {
		t.Errorf("status = %q, want running (claim already committed, 11 §3)", stub.Status)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE name = ?`, "fresh-instance").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "provisioning" {
		t.Errorf("instance state = %q, want provisioning", state)
	}
}

// TestCreateInstanceRejectsAnUnresolvableMod is Q42's request-time check. The whole point
// of validating here rather than in the job is that the caller is told before anything is
// provisioned — so the assertion is not only the 409 but the *absence* of a row: a create
// that failed on its mod list must leave no half-made server and no port claimed.
func TestCreateInstanceRejectsAnUnresolvableMod(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)

	body := validCreateBody("modded-nope")
	body["mods"] = []map[string]any{{"full_name": "Nobody-Home", "version": "1.0.0"}}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "dependency_unresolved" {
		t.Errorf("code = %q, want dependency_unresolved", got)
	}

	var rows int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM instances WHERE name = ?`, "modded-nope").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d instance rows survived a rejected create, want 0", rows)
	}
}

// TestCreateInstanceRejectsAModWithNoVersion keeps the field validation with the rest of
// the body's, so a malformed mod entry is a 422 listing the field rather than a resolver
// error about a package named "".
func TestCreateInstanceRejectsAModWithNoVersion(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)

	body := validCreateBody("modded-blank")
	body["mods"] = []map[string]any{{"full_name": "Some-Thing", "version": "  "}}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", got)
	}
}

// TestCreateInstanceCarriesModsOntoTheProvisionJob is the half of Q42 the fast suite can
// reach: the wizard's list has to survive onto the job's payload, because that payload is
// what 12 §9.2's resume rebuilds the run from. A create whose mods were validated and then
// dropped would provision, start, and generate the world vanilla — silently, which is the
// failure shape this feature exists to prevent.
func TestCreateInstanceCarriesModsOntoTheProvisionJob(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedResolvablePackage(t, db, "Someone-Thing", "1.2.3")

	body := validCreateBody("modded-yes")
	body["mods"] = []map[string]any{{"full_name": "Someone-Thing", "version": "1.2.3"}}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}

	var payload string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT j.payload FROM job_runs j JOIN instances i ON i.id = j.instance_id
		 WHERE i.name = ? AND j.kind = 'provision'`, "modded-yes").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, "Someone-Thing") {
		t.Errorf("provision payload = %s, want the chosen mod on it", payload)
	}
}

// seedResolvablePackage puts a package in the catalogue with no dependencies, plus the
// framework package the resolver adds to any closure on a vanilla instance — without that
// second row every create-with-mods resolves to dependency_unresolved on the framework
// rather than on anything the test named.
func seedResolvablePackage(t *testing.T, db *store.DB, fullName, version string) {
	t.Helper()
	packages := []store.ModPackage{
		{FullName: fullName, Namespace: "Someone", Name: "Thing", LatestVersion: version},
		{
			FullName: BepInExPack, Namespace: "denikson", Name: "BepInExPack_Valheim",
			LatestVersion: "5.4.2333",
		},
	}
	versions := []store.ModVersion{
		{FullName: fullName, Version: version, DependenciesJSON: "[]"},
		{FullName: BepInExPack, Version: "5.4.2333", DependenciesJSON: "[]"},
	}
	if err := db.UpsertModPackages(t.Context(), packages, versions); err != nil {
		t.Fatal(err)
	}
}

// fakeModEngine records what the create chain asked for and, when told to succeed, runs the
// continuation the way a finished mod_install job would.
type fakeModEngine struct {
	installed []string
	failOn    string
}

func (f *fakeModEngine) CheckResolvable(context.Context, *store.Instance, resolveRequest) error {
	return nil
}

func (f *fakeModEngine) SubmitInstall(
	ctx context.Context, _ *store.Instance, req resolveRequest,
	_ string, afterFinish func(context.Context),
) error {
	if req.FullName == f.failOn {
		return errors.New("install refused")
	}
	f.installed = append(f.installed, req.FullName)
	if afterFinish != nil {
		afterFinish(ctx)
	}
	return nil
}

// TestAfterProvisionInstallsEveryModThenStarts is Q42's ordering, which is the whole
// feature: every chosen package goes on, in the order it was chosen, and only then is the
// server started. The world is written on that first boot, so a start that overtook the
// installs would generate it vanilla and look entirely successful doing it.
func TestAfterProvisionInstallsEveryModThenStarts(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst
	engine := &fakeModEngine{}
	h.Mods = engine

	inst := seedStoppedInstance(t, db, "chain-order")
	run := &provisionRun{instanceID: inst.ID, name: inst.Name, startAfterProvision: true}
	h.installThenStart(t.Context(), run, "container-1", []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
		{FullName: "B-Two", Version: "2.0.0"},
	})

	if !reflect.DeepEqual(engine.installed, []string{"A-One", "B-Two"}) {
		t.Fatalf("installed %v, want both in order", engine.installed)
	}
	if !hasJobOfKind(t, db, inst.ID, "start") {
		t.Error("no start job after the chain finished; the wizard asked for one")
	}
}

// TestAfterProvisionDoesNotStartWhenAModFails is the other half, and the one worth the most.
// Starting anyway would hand the operator a running server with a freshly generated vanilla
// world and no sign that the mods they asked for are missing — the silent success CLAUDE.md
// §9 says to design against.
func TestAfterProvisionDoesNotStartWhenAModFails(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst
	h.Mods = &fakeModEngine{failOn: "B-Two"}

	inst := seedStoppedInstance(t, db, "chain-broken")
	run := &provisionRun{instanceID: inst.ID, name: inst.Name, startAfterProvision: true}
	h.installThenStart(t.Context(), run, "container-1", []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
		{FullName: "B-Two", Version: "2.0.0"},
	})

	if hasJobOfKind(t, db, inst.ID, "start") {
		t.Error("the server was started even though a mod failed to install")
	}
}

// TestAfterProvisionRefusesWithNoModEngine covers the wiring going missing: a panel that
// cannot install the mods must not start the server as though it had.
func TestAfterProvisionRefusesWithNoModEngine(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst
	h.Mods = nil

	inst := seedStoppedInstance(t, db, "chain-unwired")
	run := &provisionRun{instanceID: inst.ID, name: inst.Name, startAfterProvision: true}
	h.installThenStart(t.Context(), run, "container-1",
		[]resolveRequest{{FullName: "A-One", Version: "1.0.0"}})

	if hasJobOfKind(t, db, inst.ID, "start") {
		t.Error("the server was started with no mod engine to install what was asked for")
	}
}

func seedStoppedInstance(t *testing.T, db *store.DB, name string) *store.Instance {
	t.Helper()
	id := store.NewID()
	seed(t, db, `INSERT INTO instances
		(id, name, data_dir, base_port, server_name, world_name, password, public, crossplay,
		 crossplay_instance_id, state, mem_limit_mb, created_at, updated_at)
		VALUES (?, ?, ?, 2456, 'S', 'W', 'enc', 0, 0, ?, 'stopped', 2048, ?, ?)`,
		id, name, t.TempDir(), id, store.Now(), store.Now())
	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil || inst == nil {
		t.Fatalf("read seeded instance: %v", err)
	}
	return inst
}

func hasJobOfKind(t *testing.T, db *store.DB, instanceID, kind string) bool {
	t.Helper()
	var n int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE instance_id = ? AND kind = ?`,
		instanceID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}
