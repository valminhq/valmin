package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	adoptionInstanceID  = "orphan-adoptable"
	adoptionCrossplayID = "legacy-crossplay-id"
	adoptionBasePort    = 2510
	adoptionPassword    = "recover-me"
)

func adoptionBody() map[string]any {
	return map[string]any{
		"name":         "Recovered",
		"server_name":  "Recovered Server",
		"world_name":   "RecoveredWorld",
		"password":     adoptionPassword,
		"public":       true,
		"crossplay":    true,
		"preset":       "hard",
		"modifiers":    map[string]string{"combat": "hard"},
		"extra_args":   "-saveinterval 1800",
		"mem_limit_mb": 6144,
		"cpu_limit":    1.5,
	}
}

func adoptionSpec(t *testing.T, rt *Router, dataDir string) *runtime.ContainerSpec {
	t.Helper()
	cpu := 1.5
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: adoptionInstanceID, DataDir: dataDir, BasePort: adoptionBasePort,
		ServerName: "Recovered Server", WorldName: "RecoveredWorld", Password: adoptionPassword,
		Public: true, Crossplay: true, CrossplayInstanceID: adoptionCrossplayID,
		Preset: "hard", Modifiers: `{"combat":"hard"}`, ExtraArgs: "-saveinterval 1800",
		MemLimitMB: 6144, CPULimit: &cpu,
	}, rt.Supervisor().inst.Cfg.Game.Image, rt.Supervisor().inst.Cfg.Game.Network,
		rt.Supervisor().inst.Cfg.Game.StopTimeout.Std())
	if err != nil {
		t.Fatalf("build orphan container spec: %v", err)
	}
	return spec
}

func createAdoptableOrphan(
	t *testing.T, rt *Router, fake *runtime.Fake, running bool,
) (containerID, dataDir, marker string) {
	t.Helper()
	h := rt.Supervisor().inst
	dataDir = h.localDataDir(adoptionInstanceID)
	for _, name := range []string{"server", "worlds", "logs"} {
		if err := os.MkdirAll(filepath.Join(dataDir, name), 0o755); err != nil {
			t.Fatalf("create %s directory: %v", name, err)
		}
	}
	marker = filepath.Join(dataDir, "worlds", "worlds_local", "RecoveredWorld.fwl")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatalf("create world directory: %v", err)
	}
	if err := os.WriteFile(marker, []byte("existing world"), 0o644); err != nil {
		t.Fatalf("write world marker: %v", err)
	}
	if err := os.WriteFile(strings.TrimSuffix(marker, ".fwl")+".db", []byte("existing database"), 0o644); err != nil {
		t.Fatalf("write world database: %v", err)
	}
	manifest := filepath.Join(dataDir, "server", "steamapps", "appmanifest_896660.acf")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatalf("create Steam metadata directory: %v", err)
	}
	if err := os.WriteFile(manifest,
		[]byte(`"AppState" { "appid" "896660" "buildid" "123456" }`), 0o644); err != nil {
		t.Fatalf("write Steam metadata: %v", err)
	}
	var err error
	containerID, err = fake.Create(t.Context(), adoptionSpec(t, rt, h.hostDataDir(adoptionInstanceID)))
	if err != nil {
		t.Fatalf("create orphan container: %v", err)
	}
	if running {
		if err := fake.Start(t.Context(), containerID); err != nil {
			t.Fatalf("start orphan container: %v", err)
		}
	}
	return containerID, dataDir, marker
}

func TestAdoptionKeepsPanelAndHostPathsSeparate(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	h := rt.Supervisor().inst
	h.Cfg.Data.HostRoot = "/host/valmin"
	containerID, localDir, _ := createAdoptableOrphan(t, rt, fake, false)

	rec := postAdoption(t, rt, admin, containerID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("adopt = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if final := waitJob(t, rt, admin, accepted.JobID); final.Status != "succeeded" {
		t.Fatalf("adoption job = %+v, want succeeded", final)
	}

	adopted, err := db.InstanceByID(t.Context(), adoptionInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil || adopted.DataDir != localDir {
		t.Fatalf("adopted data_dir = %v, want panel path %q", adopted, localDir)
	}
	container := fake.Get(containerID)
	if container == nil || container.Spec.Binds[0].HostPath != "/host/valmin/instances/"+adoptionInstanceID+"/server" {
		t.Fatalf("container binds = %+v, want host-root source", container)
	}
}

func adoptionPath(containerID string) string {
	return "/api/v1/orphans/" + containerID
}

func postAdoption(t *testing.T, rt *Router, user *store.User, containerID string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, user, httptest.NewRequest(
		http.MethodPost, adoptionPath(containerID), jsonBody(t, adoptionBody())))
}

func adoptionInstanceCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM instances WHERE id = ?`, adoptionInstanceID).Scan(&count); err != nil {
		t.Fatalf("count instances by id: %v", err)
	}
	return count
}

func TestAdoptionPreviewAndSubmissionRequireAdmin(t *testing.T) {
	rt, _, fake, admin, member := lifecycleWorld(t)
	containerID, _, _ := createAdoptableOrphan(t, rt, fake, false)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, adoptionPath(containerID), http.NoBody),
		httptest.NewRequest(http.MethodPost, adoptionPath(containerID), jsonBody(t, adoptionBody())),
	} {
		if rec := as(rt, member, request); rec.Code != http.StatusForbidden {
			t.Errorf("member %s = %d, want 403 (%s)", request.Method, rec.Code, rec.Body)
		}
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, adoptionPath(containerID), http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin preview = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview struct {
		ContainerID         string `json:"container_id"`
		InstanceID          string `json:"instance_id"`
		CrossplayInstanceID string `json:"crossplay_instance_id"`
		BasePort            int    `json:"base_port"`
		Running             bool   `json:"running"`
	}
	decodeInto(t, rec, &preview)
	if preview.ContainerID != containerID || preview.InstanceID != adoptionInstanceID ||
		preview.CrossplayInstanceID != adoptionCrossplayID || preview.BasePort != adoptionBasePort || preview.Running {
		t.Errorf("preview = %+v, want the orphan's immutable facts", preview)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(adoptionPassword)) {
		t.Errorf("preview exposes the launch password: %s", rec.Body)
	}
}

func TestAdoptionRequiresEveryMutableField(t *testing.T) {
	fields := []string{
		"name", "server_name", "world_name", "password", "public", "crossplay",
		"preset", "modifiers", "extra_args", "mem_limit_mb", "cpu_limit",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			rt, db, fake, admin, _ := lifecycleWorld(t)
			containerID, _, _ := createAdoptableOrphan(t, rt, fake, false)
			body := adoptionBody()
			delete(body, field)

			rec := as(rt, admin, httptest.NewRequest(
				http.MethodPost, adoptionPath(containerID), jsonBody(t, body)))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("POST without %s = %d, want 422 (%s)", field, rec.Code, rec.Body)
			}
			if code := errCode(t, rec); code != "validation_failed" {
				t.Errorf("POST without %s code = %q, want validation_failed", field, code)
			}
			if got := adoptionInstanceCount(t, db); got != 0 {
				t.Errorf("POST without %s published %d rows, want 0", field, got)
			}
			if fake.Get(containerID) == nil {
				t.Errorf("POST without %s removed the orphan", field)
			}
		})
	}
}

func TestAdoptionPreservesTheRunningContainerAndFilesystem(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID, dataDir, marker := createAdoptableOrphan(t, rt, fake, true)
	runsBefore := fake.Runs()
	containersBefore, err := fake.List(t.Context(), map[string]string{instance.LabelManaged: "true"})
	if err != nil {
		t.Fatal(err)
	}

	rec := postAdoption(t, rt, admin, containerID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("adopt = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want a job resource", location)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(adoptionPassword)) {
		t.Errorf("adoption response exposes the launch password: %s", rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if accepted.Kind != "adopt" {
		t.Errorf("job kind = %q, want adopt", accepted.Kind)
	}
	if accepted.InstanceID == nil || *accepted.InstanceID != adoptionInstanceID {
		t.Fatalf("job instance_id = %v, want %q", accepted.InstanceID, adoptionInstanceID)
	}
	if final := waitJob(t, rt, admin, accepted.JobID); final.Status != "succeeded" {
		t.Fatalf("adoption job = %+v, want succeeded", final)
	}

	adopted, err := db.InstanceByID(t.Context(), adoptionInstanceID)
	if err != nil || adopted == nil {
		t.Fatalf("read adopted instance: %v", err)
	}
	if adopted.ContainerID == nil || *adopted.ContainerID != containerID {
		t.Errorf("container_id = %v, want preserved %q", adopted.ContainerID, containerID)
	}
	if adopted.State != "running" || adopted.DataDir != dataDir || adopted.BasePort != adoptionBasePort ||
		adopted.CrossplayInstanceID != adoptionCrossplayID {
		t.Errorf("adopted identity = %+v, want the running orphan's identity", adopted)
	}
	if adopted.Name != "Recovered" || adopted.ServerName != "Recovered Server" ||
		adopted.WorldName != "RecoveredWorld" || !adopted.Public || !adopted.Crossplay ||
		adopted.Preset == nil || *adopted.Preset != "hard" ||
		adopted.Modifiers == nil || *adopted.Modifiers != `{"combat":"hard"}` ||
		adopted.ExtraArgs == nil || *adopted.ExtraArgs != "-saveinterval 1800" ||
		adopted.MemLimitMB != 6144 || adopted.CPULimit == nil || *adopted.CPULimit != 1.5 {
		t.Errorf("adopted launch config = %+v, want the submitted fields", adopted)
	}

	envelope, err := db.InstancePassword(t.Context(), adoptionInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := rt.Supervisor().inst.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(adoptionInstanceID),
		envelope,
	)
	if err != nil {
		t.Fatalf("decrypt adopted password: %v", err)
	}
	if string(plaintext) != adoptionPassword {
		t.Errorf("adopted password = %q, want submitted password", plaintext)
	}

	var payload string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT payload FROM job_runs WHERE id = ?`, accepted.JobID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, adoptionPassword) {
		t.Errorf("durable job payload exposes the launch password: %s", payload)
	}
	if fake.Runs() != runsBefore {
		t.Errorf("runtime starts = %d, want unchanged %d", fake.Runs(), runsBefore)
	}
	container := fake.Get(containerID)
	if container == nil || !container.Running {
		t.Fatalf("running orphan was stopped, removed, or replaced: %+v", container)
	}
	containersAfter, err := fake.List(t.Context(), map[string]string{instance.LabelManaged: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(containersAfter) != len(containersBefore) {
		t.Errorf("managed containers = %d, want unchanged %d", len(containersAfter), len(containersBefore))
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "existing world" {
		t.Errorf("existing world marker = %q, %v; want unchanged", got, err)
	}

	audit, err := db.ListAuditLog(t.Context(), store.AuditFilter{
		InstanceID: adoptionInstanceID, UserID: admin.ID, Action: "instance.adopt",
	}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].UserID == nil || *audit[0].UserID != admin.ID {
		t.Fatalf("adoption audit = %+v, want one entry attributed to %q", audit, admin.ID)
	}
	if audit[0].Detail == nil || !strings.Contains(*audit[0].Detail, containerID) ||
		strings.Contains(*audit[0].Detail, adoptionPassword) {
		t.Errorf("adoption audit detail = %v, want container id and no password", audit[0].Detail)
	}

	repeat := postAdoption(t, rt, admin, containerID)
	if repeat.Code == http.StatusAccepted {
		t.Errorf("repeat adoption = 202, want refusal (%s)", repeat.Body)
	}
	if got := adoptionInstanceCount(t, db); got != 1 {
		t.Errorf("rows for adopted id = %d, want 1", got)
	}
}

func TestAdoptionRejectsUnverifiedContainersBeforePublishingARow(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*runtime.FakeContainer)
		seedCollision func(*testing.T, *store.DB)
		wantRows      int
	}{
		{
			name: "unmanaged",
			mutate: func(c *runtime.FakeContainer) {
				delete(c.Labels, instance.LabelManaged)
				delete(c.Spec.Labels, instance.LabelManaged)
			},
		},
		{
			name: "missing instance id",
			mutate: func(c *runtime.FakeContainer) {
				delete(c.Labels, instance.LabelInstanceID)
				delete(c.Spec.Labels, instance.LabelInstanceID)
			},
		},
		{
			name: "missing base port",
			mutate: func(c *runtime.FakeContainer) {
				delete(c.Labels, instance.LabelBasePort)
				delete(c.Spec.Labels, instance.LabelBasePort)
			},
		},
		{
			name: "missing spec hash",
			mutate: func(c *runtime.FakeContainer) {
				delete(c.Labels, instance.LabelSpecHash)
				delete(c.Spec.Labels, instance.LabelSpecHash)
			},
		},
		{
			name: "foreign mount",
			mutate: func(c *runtime.FakeContainer) {
				c.Spec.Binds[0].HostPath = "/foreign/server"
			},
		},
		{
			name: "wrong process user",
			mutate: func(c *runtime.FakeContainer) {
				c.Spec.User = "0:0"
			},
		},
		{
			name: "privileged security mode",
			mutate: func(c *runtime.FakeContainer) {
				c.Security.Privileged = true
			},
		},
		{
			name: "non-bind mount",
			mutate: func(c *runtime.FakeContainer) {
				c.Security.NonBindMount = true
			},
		},
		{
			name: "wrong published port",
			mutate: func(c *runtime.FakeContainer) {
				c.Spec.Ports[0].HostPort++
			},
		},
		{
			name: "duplicate instance id",
			seedCollision: func(t *testing.T, db *store.DB) {
				seedAdoptionCollision(t, db, adoptionInstanceID, 2600)
			},
			wantRows: 1,
		},
		{
			name: "duplicate base port",
			seedCollision: func(t *testing.T, db *store.DB) {
				seedAdoptionCollision(t, db, "different-instance", adoptionBasePort)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt, db, fake, admin, _ := lifecycleWorld(t)
			containerID, _, _ := createAdoptableOrphan(t, rt, fake, false)
			if test.mutate != nil {
				test.mutate(fake.Get(containerID))
			}
			if test.seedCollision != nil {
				test.seedCollision(t, db)
			}

			rec := postAdoption(t, rt, admin, containerID)
			if rec.Code == http.StatusAccepted {
				var accepted jobView
				decodeInto(t, rec, &accepted)
				if final := waitJob(t, rt, admin, accepted.JobID); final.Status != "failed" {
					t.Fatalf("invalid adoption job = %+v, want failed", final)
				}
			}
			if got := adoptionInstanceCount(t, db); got != test.wantRows {
				t.Errorf("rows for orphan identity = %d, want %d", got, test.wantRows)
			}
			if fake.Get(containerID) == nil {
				t.Error("refused adoption removed the container")
			}
		})
	}
}

func TestAdoptionRejectsASymlinkedWorldDirectory(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID, dataDir, _ := createAdoptableOrphan(t, rt, fake, false)
	worldsLocal := filepath.Join(dataDir, "worlds", "worlds_local")
	realWorlds := filepath.Join(dataDir, "worlds", "stored-worlds")
	if err := os.Rename(worldsLocal, realWorlds); err != nil {
		t.Fatalf("move world directory: %v", err)
	}
	if err := os.Symlink(realWorlds, worldsLocal); err != nil {
		t.Fatalf("symlink world directory: %v", err)
	}

	rec := postAdoption(t, rt, admin, containerID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("adopt with symlinked worlds_local = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if code := errCode(t, rec); code != "container_mismatch" {
		t.Errorf("error code = %q, want container_mismatch", code)
	}
	if got := adoptionInstanceCount(t, db); got != 0 {
		t.Errorf("symlinked world directory published %d rows, want 0", got)
	}
	if fake.Get(containerID) == nil {
		t.Error("refused adoption removed the orphan")
	}
}

func TestAdoptionRejectsAContainerClaimedByAnotherInstance(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID, _, _ := createAdoptableOrphan(t, rt, fake, false)
	seed(t, db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (
		'other-instance', 'Other instance', 'stopped', ?, '/existing/other', 2600,
		'Existing', 'ExistingWorld', 'envelope', 'other-crossplay', ?, ?
	)`, containerID, store.Now(), store.Now())

	rec := postAdoption(t, rt, admin, containerID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("adopt claimed container = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if code := errCode(t, rec); code != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", code)
	}
	if got := adoptionInstanceCount(t, db); got != 0 {
		t.Errorf("claimed container published %d rows for its label identity, want 0", got)
	}
	var claims int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM instances WHERE container_id = ?`, containerID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 1 {
		t.Errorf("container claims = %d, want the existing row only", claims)
	}
	if fake.Get(containerID) == nil {
		t.Error("refused adoption removed the container")
	}
}

func seedAdoptionCollision(t *testing.T, db *store.DB, id string, basePort int) {
	t.Helper()
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, 'Existing', 'ExistingWorld', 'envelope', ?, ?, ?)`,
		id, "existing-"+id, "/existing/"+id, basePort, "crossplay-"+id, store.Now(), store.Now())
}

func TestRecoveryLeavesAnInterruptedAdoptionContainerIntact(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID, _, marker := createAdoptableOrphan(t, rt, fake, true)
	jobID := store.NewID()
	payload, err := json.Marshal(map[string]string{"container_id": containerID})
	if err != nil {
		t.Fatal(err)
	}
	lockKey := "container:" + containerID
	seed(t, db, `INSERT INTO job_runs (
		id, kind, status, lock_key, instance_name, payload, requested_by, lease_owner, lease_until,
		created_at, started_at
	) VALUES (?, 'adopt', 'running', ?, 'Recovered', ?, ?, 'panel:dead-boot', ?, ?, ?)`,
		jobID, lockKey, string(payload), admin.ID, store.FormatTime(time.Now().Add(time.Minute)),
		store.Now(), store.Now())
	seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		lockKey, jobID, store.Now())

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	container := fake.Get(containerID)
	if container == nil || !container.Running {
		t.Fatalf("interrupted adoption removed or stopped its container: %+v", container)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "existing world" {
		t.Errorf("world marker = %q, %v; want unchanged", got, err)
	}
	if got := adoptionInstanceCount(t, db); got != 0 {
		t.Errorf("recovery published %d instance rows, want 0", got)
	}
	job, err := db.JobByID(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "failed" || job.ErrorCode == nil || *job.ErrorCode != "interrupted" {
		t.Errorf("recovered job = %+v, want failed/interrupted", job)
	}
	var locks int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_locks WHERE job_id = ?`, jobID).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if locks != 0 {
		t.Errorf("locks after recovery = %d, want 0", locks)
	}
}
