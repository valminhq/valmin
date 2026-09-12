package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	cloneSourceID       = "clone-source"
	cloneSourcePassword = "source-secret"
)

func clonePath(sourceID string) string { return "/api/v1/instances/" + sourceID + "/clone" }

func seedCloneSource(t *testing.T, rt *Router, db *store.DB, state string) *store.Instance {
	t.Helper()
	dataDir := filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot, "instances", cloneSourceID)
	envelope, err := rt.Supervisor().inst.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(cloneSourceID),
		[]byte(cloneSourcePassword),
	)
	if err != nil {
		t.Fatal(err)
	}
	cpuLimit := 1.75
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		public, crossplay, crossplay_instance_id, preset, modifiers, extra_args,
		modded, bepinex_version, restart_required, mem_limit_mb, cpu_limit,
		game_build_id, backup_keep_cold, backup_keep_hot, backup_on_restart,
		created_at, updated_at
	) VALUES (?, 'source', ?, ?, 2500, 'Source Server', 'SourceWorld', ?,
		1, 1, 'source-crossplay', 'hard', '{"combat":"hard"}', '-logFile /opt/valheim/logs/custom.log',
		1, '5.4.2333', 1, 6144, ?, '123456', 7, 3, 1, ?, ?)`,
		cloneSourceID, state, dataDir, envelope, cpuLimit, store.Now(), store.Now())

	serverDir := instance.ServerDir(dataDir)
	worldDir := filepath.Join(instance.WorldsDir(dataDir), "worlds_local")
	for _, dir := range []string{filepath.Join(serverDir, "BepInEx", "config"), worldDir} {
		if err := os.MkdirAll(dir, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		filepath.Join(serverDir, "valheim_server.x86_64"): []byte("server binary"),
		filepath.Join(serverDir, "steamapps", "appmanifest_896660.acf"): []byte(
			`"AppState" { "appid" "896660" "buildid" "123456" }`),
		filepath.Join(serverDir, "BepInEx", "config", "example.cfg"):    []byte("setting = source"),
		filepath.Join(serverDir, "BepInEx", "plugins", "example.dll"):   []byte("plugin bytes"),
		filepath.Join(worldDir, "SourceWorld.db"):                       dbBytes(),
		filepath.Join(worldDir, "SourceWorld.fwl"):                      fwlBytes(37, "SourceWorld"),
		filepath.Join(instance.WorldsDir(dataDir), "adminlist.txt"):     []byte("admin-id\n"),
		filepath.Join(instance.WorldsDir(dataDir), "permittedlist.txt"): []byte("friend-id\n"),
		filepath.Join(instance.WorldsDir(dataDir), "bannedlist.txt"):    []byte("banned-id\n"),
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o664); err != nil {
			t.Fatal(err)
		}
	}
	seed(t, db, `INSERT INTO instance_mods (
		instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at
	) VALUES (?, 'Author-Example', '1.2.3', 'explicit', 'server_only', 1,
		?, ?)`, cloneSourceID,
		`[{"path":"BepInEx/plugins/example.dll","sha256":"abc"}]`, store.Now())

	inst, err := db.InstanceByID(t.Context(), cloneSourceID)
	if err != nil || inst == nil {
		t.Fatalf("read clone source: %v", err)
	}
	return inst
}

func postClone(t *testing.T, rt *Router, u *store.User, name string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodPost, clonePath(cloneSourceID), jsonBody(t, map[string]string{"name": name})))
}

func countInstancesNamed(t *testing.T, db *store.DB, name string) int {
	t.Helper()
	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM instances WHERE name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func sameOptionalString(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func TestCloneCancelPolicyClosesAtContainerCreation(t *testing.T) {
	for _, checkpoint := range []string{"", "dirs_created", "server_cloned", "world_archived", "world_restored"} {
		if ok, phase := cloneCancelPolicy(checkpoint); !ok || phase != "" {
			t.Errorf("cloneCancelPolicy(%q) = %v, %q; want cancellable", checkpoint, ok, phase)
		}
	}
	if ok, phase := cloneCancelPolicy("container_created"); ok || phase != "container_created" {
		t.Errorf("cloneCancelPolicy(container_created) = %v, %q; want false, container_created", ok, phase)
	}
}

func TestCloneRequiresAdmin(t *testing.T) {
	rt, db, _, member := provisionWorld(t)
	seedCloneSource(t, rt, db, string(instance.StateStopped))
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES (?, ?, 'operator', '["instance.clone"]', ?)`, member.ID, cloneSourceID, store.Now())

	rec := postClone(t, rt, member, "forbidden-copy")
	if rec.Code != http.StatusForbidden {
		t.Errorf("clone as member = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if got := countInstancesNamed(t, db, "forbidden-copy"); got != 0 {
		t.Errorf("clone created %d destination rows before authorization, want 0", got)
	}
}

func TestCloneRefusesRunningSourceWithoutStoppingIt(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedCloneSource(t, rt, db, string(instance.StateRunning))

	rec := postClone(t, rt, admin, "running-copy")
	if rec.Code != http.StatusConflict {
		t.Fatalf("clone running source = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", got)
	}
	source, err := db.InstanceByID(t.Context(), cloneSourceID)
	if err != nil {
		t.Fatal(err)
	}
	if source.State != string(instance.StateRunning) {
		t.Errorf("source state = %q, want running", source.State)
	}
	if got := countInstancesNamed(t, db, "running-copy"); got != 0 {
		t.Errorf("refused clone left %d destination rows, want 0", got)
	}
}

func TestCloneRefusesLockedSourceWithoutLeakingDestination(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedCloneSource(t, rt, db, string(instance.StateStopped))

	release := make(chan struct{})
	started := make(chan struct{})
	holder, err := rt.Supervisor().inst.Engine.Submit(t.Context(), &jobs.Spec{
		Kind:       jobs.KindBackup,
		LockKey:    jobs.InstanceLockKey(cloneSourceID),
		InstanceID: ptr(cloneSourceID),
	}, func(context.Context, *jobs.Handle) jobs.Outcome {
		close(started)
		<-release
		return jobs.Outcome{Status: "succeeded"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	defer func() {
		close(release)
		waitJob(t, rt, admin, holder.ID)
	}()

	rec := postClone(t, rt, admin, "locked-copy")
	if rec.Code != http.StatusConflict {
		t.Fatalf("clone locked source = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "job_in_progress" {
		t.Errorf("error code = %q, want job_in_progress", got)
	}
	if !strings.Contains(rec.Body.String(), holder.ID) {
		t.Errorf("conflict response does not identify holder %q: %s", holder.ID, rec.Body)
	}
	if got := countInstancesNamed(t, db, "locked-copy"); got != 0 {
		t.Errorf("conflicted clone left %d destination rows, want 0", got)
	}
}

func TestCloneReturnsAJobForAFreshDestination(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	sourceBefore := seedCloneSource(t, rt, db, string(instance.StateStopped))
	sourceEnvelope, err := db.InstancePassword(t.Context(), cloneSourceID)
	if err != nil {
		t.Fatal(err)
	}

	rec := postClone(t, rt, admin, "source-copy")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("clone = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want a job resource", loc)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(cloneSourcePassword)) ||
		bytes.Contains(rec.Body.Bytes(), []byte(sourceEnvelope)) ||
		bytes.Contains(rec.Body.Bytes(), []byte("password")) {
		t.Errorf("clone response exposes password material: %s", rec.Body)
	}

	var stub jobView
	decodeInto(t, rec, &stub)
	if stub.Kind != "clone" {
		t.Errorf("kind = %q, want clone", stub.Kind)
	}
	if stub.InstanceID == nil || *stub.InstanceID == "" || *stub.InstanceID == cloneSourceID {
		t.Fatalf("destination instance_id = %v, want a fresh id", stub.InstanceID)
	}

	destination, err := db.InstanceByID(t.Context(), *stub.InstanceID)
	if err != nil || destination == nil {
		t.Fatalf("read destination: %v", err)
	}
	if destination.Name != "source-copy" {
		t.Errorf("destination name = %q, want source-copy", destination.Name)
	}
	if destination.BasePort == sourceBefore.BasePort {
		t.Errorf("destination reused source base port %d", destination.BasePort)
	}
	if destination.CrossplayInstanceID != destination.ID ||
		destination.CrossplayInstanceID == sourceBefore.CrossplayInstanceID {
		t.Errorf("destination crossplay id = %q, want fresh instance id %q",
			destination.CrossplayInstanceID, destination.ID)
	}
	if destination.DataDir == sourceBefore.DataDir || !strings.HasSuffix(destination.DataDir, destination.ID) {
		t.Errorf("destination data_dir = %q, source = %q", destination.DataDir, sourceBefore.DataDir)
	}
	if destination.ServerName != sourceBefore.ServerName ||
		destination.WorldName != sourceBefore.WorldName ||
		destination.Public != sourceBefore.Public ||
		destination.Crossplay != sourceBefore.Crossplay ||
		!sameOptionalString(destination.Preset, sourceBefore.Preset) ||
		!sameOptionalString(destination.Modifiers, sourceBefore.Modifiers) ||
		!sameOptionalString(destination.ExtraArgs, sourceBefore.ExtraArgs) ||
		destination.Modded != sourceBefore.Modded ||
		!sameOptionalString(destination.BepInExVersion, sourceBefore.BepInExVersion) ||
		!sameOptionalString(destination.GameBuildID, sourceBefore.GameBuildID) ||
		destination.MemLimitMB != sourceBefore.MemLimitMB ||
		(destination.CPULimit == nil || *destination.CPULimit != *sourceBefore.CPULimit) ||
		destination.BackupKeepCold != sourceBefore.BackupKeepCold ||
		destination.BackupKeepHot != sourceBefore.BackupKeepHot ||
		destination.BackupOnRestart != sourceBefore.BackupOnRestart {
		t.Errorf("destination did not preserve source launch settings: source=%+v destination=%+v",
			sourceBefore, destination)
	}

	destinationEnvelope, err := db.InstancePassword(t.Context(), destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	if destinationEnvelope == sourceEnvelope {
		t.Error("destination reused ciphertext bound to the source row")
	}
	plaintext, err := rt.Supervisor().inst.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(destination.ID),
		destinationEnvelope,
	)
	if err != nil {
		t.Fatalf("decrypt destination password: %v", err)
	}
	if string(plaintext) != cloneSourcePassword {
		t.Errorf("destination password = %q, want source password", plaintext)
	}

	var payload string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT payload FROM job_runs WHERE id = ?`, stub.JobID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, cloneSourcePassword) || strings.Contains(payload, sourceEnvelope) ||
		strings.Contains(payload, destinationEnvelope) {
		t.Errorf("durable job payload exposes password material: %s", payload)
	}
	auditRows, err := db.ListAuditLog(t.Context(), store.AuditFilter{
		InstanceID: cloneSourceID,
		UserID:     admin.ID,
		Action:     "instance.clone",
	}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("clone audit entries = %d, want 1", len(auditRows))
	}
	audit := auditRows[0]
	if audit.UserID == nil || *audit.UserID != admin.ID ||
		audit.InstanceID == nil || *audit.InstanceID != cloneSourceID ||
		audit.Action != "instance.clone" {
		t.Errorf("clone audit attribution = %+v, want requester %q and source %q",
			audit, admin.ID, cloneSourceID)
	}
	if audit.Detail == nil || !strings.Contains(*audit.Detail, cloneSourceID) ||
		!strings.Contains(*audit.Detail, destination.ID) {
		t.Errorf("clone audit detail = %v, want source %q and destination %q",
			audit.Detail, cloneSourceID, destination.ID)
	}
	if audit.Detail != nil && (strings.Contains(*audit.Detail, cloneSourcePassword) ||
		strings.Contains(*audit.Detail, sourceEnvelope) ||
		strings.Contains(*audit.Detail, destinationEnvelope) ||
		strings.Contains(*audit.Detail, "password")) {
		t.Errorf("clone audit detail exposes password material: %s", *audit.Detail)
	}
	sourceAfter, err := db.InstanceByID(t.Context(), cloneSourceID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter.State != sourceBefore.State || sourceAfter.ContainerID != sourceBefore.ContainerID ||
		sourceAfter.BasePort != sourceBefore.BasePort || sourceAfter.CrossplayInstanceID != sourceBefore.CrossplayInstanceID {
		t.Errorf("clone mutated source identity or state: before=%+v after=%+v", sourceBefore, sourceAfter)
	}
}

func TestCloneWrongUIDFailsBeforeCopying(t *testing.T) {
	if os.Getuid() == instance.WantCloneUID {
		t.Skip("requires a process uid other than the panel uid")
	}
	rt, db, admin, _ := provisionWorld(t)
	seedCloneSource(t, rt, db, string(instance.StateStopped))

	rec := postClone(t, rt, admin, "wrong-uid-copy")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("clone = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("clone job = %+v, want failed", final)
	}
	if stub.InstanceID == nil {
		t.Fatal("clone job has no destination instance id")
	}
	destination, err := db.InstanceByID(t.Context(), *stub.InstanceID)
	if err != nil || destination == nil {
		t.Fatalf("read failed destination: %v", err)
	}
	if destination.State != string(instance.StateError) {
		t.Errorf("destination state = %q, want error", destination.State)
	}
	if destination.ContainerID != nil {
		t.Errorf("failed destination has container %q", *destination.ContainerID)
	}
	if _, err := os.Stat(instance.ServerDir(destination.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination server tree exists after pre-copy uid failure: %v", err)
	}
}

func TestCloneCopiesWorldConfigurationAndModManifest(t *testing.T) {
	if os.Getuid() != instance.WantCloneUID {
		t.Skip("clone success requires the panel uid")
	}
	rt, db, admin, _ := provisionWorld(t)
	source := seedCloneSource(t, rt, db, string(instance.StateStopped))
	cacheMarker := filepath.Join(rt.Supervisor().inst.Cfg.Data.Root, "cache", "pristine")
	if err := os.MkdirAll(filepath.Dir(cacheMarker), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheMarker, []byte("cache bytes"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBackup(t.Context(), &store.Backup{
		ID: "source-backup", InstanceID: source.ID,
		Path:      filepath.Join(rt.Supervisor().inst.Cfg.Data.Root, "backups", "source.tar.gz"),
		SizeBytes: 4096, SHA256: strings.Repeat("a", 64), WorldName: source.WorldName,
		Trigger: store.TriggerManual, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}

	rec := postClone(t, rt, admin, "complete-copy")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("clone = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "succeeded" {
		t.Fatalf("clone job = %+v, want succeeded", final)
	}
	if stub.InstanceID == nil {
		t.Fatal("clone job has no destination instance id")
	}
	destination, err := db.InstanceByID(t.Context(), *stub.InstanceID)
	if err != nil || destination == nil {
		t.Fatalf("read destination: %v", err)
	}
	if destination.State != string(instance.StateStopped) {
		t.Errorf("destination state = %q, want stopped", destination.State)
	}
	if destination.ContainerID == nil {
		t.Fatal("destination has no container")
	}
	container, err := rt.Supervisor().inst.Runtime.Inspect(t.Context(), *destination.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if container.Running {
		t.Error("clone started the destination container")
	}
	if container.Labels[instance.LabelInstanceID] != destination.ID {
		t.Errorf("container labels = %v, want destination id %q", container.Labels, destination.ID)
	}

	for rel, want := range map[string][]byte{
		"server/BepInEx/config/example.cfg":   []byte("setting = source"),
		"server/BepInEx/plugins/example.dll":  []byte("plugin bytes"),
		"worlds/worlds_local/SourceWorld.db":  dbBytes(),
		"worlds/worlds_local/SourceWorld.fwl": fwlBytes(37, "SourceWorld"),
		"worlds/adminlist.txt":                []byte("admin-id\n"),
		"worlds/permittedlist.txt":            []byte("friend-id\n"),
		"worlds/bannedlist.txt":               []byte("banned-id\n"),
	} {
		got, err := os.ReadFile(filepath.Join(destination.DataDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read cloned %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("cloned %s = %q, want %q", rel, got, want)
		}
	}

	sourceMods, err := db.InstanceMods(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	destinationMods, err := db.InstanceMods(t.Context(), destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceMods) != 1 || len(destinationMods) != 1 {
		t.Fatalf("source mods = %d, destination mods = %d, want one each",
			len(sourceMods), len(destinationMods))
	}
	sm, dm := sourceMods[0], destinationMods[0]
	if dm.InstanceID != destination.ID || dm.FullName != sm.FullName || dm.Version != sm.Version ||
		dm.InstalledAs != sm.InstalledAs || dm.Side != sm.Side || dm.Enabled != sm.Enabled ||
		dm.FileManifest != sm.FileManifest || dm.InstalledAt != sm.InstalledAt {
		t.Errorf("destination mod = %+v, want source manifest %+v with destination id", dm, sm)
	}
	sourceBackups, err := db.ListBackups(t.Context(), source.ID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	destinationBackups, err := db.ListBackups(t.Context(), destination.ID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceBackups) != 1 || sourceBackups[0].ID != "source-backup" {
		t.Errorf("source backup catalogue = %+v, want original row", sourceBackups)
	}
	if len(destinationBackups) != 1 || destinationBackups[0].ID == "source-backup" ||
		destinationBackups[0].Trigger != store.TriggerManual || !destinationBackups[0].Consistent {
		t.Errorf("destination backup catalogue = %+v, want one independent seed archive", destinationBackups)
	}

	destinationConfig := filepath.Join(destination.DataDir, "server", "BepInEx", "config", "example.cfg")
	if err := os.WriteFile(destinationConfig, []byte("setting = destination"), 0o664); err != nil {
		t.Fatal(err)
	}
	destinationWorld := filepath.Join(destination.DataDir, "worlds", "worlds_local", "SourceWorld.db")
	if err := os.WriteFile(destinationWorld, []byte("destination world"), 0o664); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		filepath.Join(source.DataDir, "server", "BepInEx", "config", "example.cfg"): []byte("setting = source"),
		filepath.Join(source.DataDir, "worlds", "worlds_local", "SourceWorld.db"):   dbBytes(),
		cacheMarker: []byte("cache bytes"),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("isolated source %s changed to %q, want %q", path, got, want)
		}
	}
}

func seedInterruptedClone(
	t *testing.T, rt *Router, db *store.DB, checkpoint string,
) (destinationID, deadJobID string) {
	t.Helper()
	seedCloneSource(t, rt, db, string(instance.StateStopped))
	destinationID = "clone-destination"
	envelope, err := rt.Supervisor().inst.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(destinationID),
		[]byte(cloneSourcePassword),
	)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.TxCreateCloneInstance(t.Context(), tx, cloneSourceID, &store.NewInstance{
		ID: destinationID, Name: "interrupted-copy",
		DataDir:             filepath.Join(rt.Supervisor().inst.Cfg.Data.HostRoot, "instances", destinationID),
		BasePort:            2600,
		Password:            envelope,
		CrossplayInstanceID: destinationID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	archiveID := "clone-seed-archive"
	archivePath := filepath.Join(instance.BackupsDir(rt.Supervisor().inst.Cfg.Data.Root),
		destinationID, "clone-seed.tar.gz")
	payload, err := json.Marshal(clonePayload{
		SourceID: cloneSourceID, ArchiveID: archiveID, ArchivePath: archivePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadJobID = "dead-clone-job"
	var checkpointValue any
	if checkpoint != "" {
		checkpointValue = checkpoint
	}
	seed(t, db, `INSERT INTO job_runs (
		id, kind, status, lock_key, instance_id, instance_name, payload, checkpoint,
		lease_owner, lease_until, created_at, started_at
	) VALUES (?, 'clone', 'running', ?, ?, 'interrupted-copy', ?, ?,
		'panel:dead-boot', ?, ?, ?)`, deadJobID, jobs.InstanceLockKey(destinationID),
		destinationID, string(payload), checkpointValue,
		store.FormatTime(time.Now().Add(time.Hour)), store.Now(), store.Now())
	for _, key := range []string{jobs.InstanceLockKey(destinationID), jobs.InstanceLockKey(cloneSourceID)} {
		seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
			key, deadJobID, store.Now())
	}
	return destinationID, deadJobID
}

func TestRecoverParksAnUncheckpointedCloneAndReleasesBothLocks(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	destinationID, deadJobID := seedInterruptedClone(t, rt, db, "")

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	destination, err := db.InstanceByID(t.Context(), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	if destination.State != string(instance.StateError) {
		t.Errorf("destination state = %q, want error", destination.State)
	}
	dead, err := db.JobByID(t.Context(), deadJobID)
	if err != nil {
		t.Fatal(err)
	}
	if dead.Status != "failed" || dead.ErrorCode == nil || *dead.ErrorCode != "interrupted" {
		t.Errorf("dead clone job = %+v, want failed/interrupted", dead)
	}
	held, err := db.HeldLockKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{jobs.InstanceLockKey(destinationID), jobs.InstanceLockKey(cloneSourceID)} {
		if held[key] {
			t.Errorf("recovery left lock %q held", key)
		}
	}
	if rt.Supervisor().inst.Runtime.(*runtime.Fake).Runs() != 0 {
		t.Error("recovery started a server")
	}
}

func TestRecoverParksACheckpointedCloneAndResolvesStaging(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	destinationID, deadJobID := seedInterruptedClone(t, rt, db, "dirs_created")
	destination, err := db.InstanceByID(t.Context(), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	liveWorlds := instance.WorldsDir(destination.DataDir)
	stagedWorlds := liveWorlds + backup.StagedSuffix
	if err := os.MkdirAll(liveWorlds, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagedWorlds, 0o775); err != nil {
		t.Fatal(err)
	}
	liveMarker := filepath.Join(liveWorlds, "live.txt")
	if err := os.WriteFile(liveMarker, []byte("live"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedWorlds, "staged.txt"), []byte("staged"), 0o664); err != nil {
		t.Fatal(err)
	}
	archivePart := filepath.Join(instance.BackupsDir(rt.Supervisor().inst.Cfg.Data.Root),
		destinationID, "clone-seed.tar.gz"+backup.PartSuffix)
	if err := os.MkdirAll(filepath.Dir(archivePart), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePart, []byte("partial archive"), 0o664); err != nil {
		t.Fatal(err)
	}

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	destination, err = db.InstanceByID(t.Context(), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	if destination.State != string(instance.StateError) {
		t.Errorf("destination state = %q, want error", destination.State)
	}
	dead, err := db.JobByID(t.Context(), deadJobID)
	if err != nil {
		t.Fatal(err)
	}
	if dead.Status != "failed" || dead.ErrorCode == nil || *dead.ErrorCode != "interrupted" {
		t.Errorf("dead clone job = %+v, want failed/interrupted", dead)
	}
	var cloneJobs int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'clone'`).Scan(&cloneJobs); err != nil {
		t.Fatal(err)
	}
	if cloneJobs != 1 {
		t.Errorf("clone jobs after recovery = %d, want only the interrupted job", cloneJobs)
	}
	source, err := db.InstanceByID(t.Context(), cloneSourceID)
	if err != nil {
		t.Fatal(err)
	}
	if source.State != string(instance.StateStopped) {
		t.Errorf("source state = %q, want stopped", source.State)
	}
	held, err := db.HeldLockKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{jobs.InstanceLockKey(destinationID), jobs.InstanceLockKey(cloneSourceID)} {
		if held[key] {
			t.Errorf("recovery left lock %q held", key)
		}
	}
	if got, err := os.ReadFile(liveMarker); err != nil || string(got) != "live" {
		t.Errorf("live world marker after recovery = %q, %v; want live", got, err)
	}
	for _, path := range []string{stagedWorlds, liveWorlds + backup.SupersededSuffix, archivePart} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("staging path %q remains after recovery: %v", path, err)
		}
	}
	fakeRuntime := rt.Supervisor().inst.Runtime.(*runtime.Fake)
	containers, err := fakeRuntime.List(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 0 {
		t.Errorf("containers after recovery = %d, want 0", len(containers))
	}
	if fakeRuntime.Runs() != 0 {
		t.Error("clone recovery started a server")
	}
}
