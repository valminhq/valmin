//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

func TestCloneCrashParksDestinationAndReleasesLocks(t *testing.T) {
	if os.Geteuid() != instance.WantCloneUID {
		t.Skipf("clone crash recovery requires uid %d", instance.WantCloneUID)
	}

	p := newPanel(t, nil)
	d := docker(t)
	sourceID, sourceContainerID := seedInstance(t, p, d, "clone-crash")
	sourceDir := filepath.Join(p.root, "instances", sourceID)
	writeCloneSourceServer(t, sourceDir)
	worldDir := filepath.Join(sourceDir, "worlds", "worlds_local")
	worldBefore := writeCrashWorld(t, worldDir)
	serverBefore := treeHash(t, sourceDir)

	p.start()
	p.setup()
	db := openPanelDB(t, p)
	defer func() { _ = db.Close() }()
	sourceBefore, err := db.InstanceByID(t.Context(), sourceID)
	if err != nil || sourceBefore == nil {
		t.Fatalf("read source before clone: %+v, %v", sourceBefore, err)
	}
	if sourceBefore.State != "stopped" || inspect(t, d, sourceContainerID).Running {
		t.Fatalf("source is not stopped before clone: %+v", sourceBefore)
	}
	passwordBefore, err := db.InstancePassword(t.Context(), sourceID)
	if err != nil {
		t.Fatalf("read source password before clone: %v", err)
	}

	resp := p.do(http.MethodPost, "/api/v1/instances/"+sourceID+"/clone",
		map[string]string{"name": "crash-copy-" + suffix()})
	if resp.status != http.StatusAccepted {
		t.Fatalf("clone = %d, want 202 (%s)", resp.status, resp.body)
	}
	var accepted struct {
		JobID      string  `json:"job_id"`
		InstanceID *string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(resp.body), &accepted); err != nil {
		t.Fatalf("decode clone response %q: %v", resp.body, err)
	}
	if accepted.JobID == "" || accepted.InstanceID == nil || *accepted.InstanceID == "" {
		t.Fatalf("clone response has no job or destination: %s", resp.body)
	}
	destinationID := *accepted.InstanceID
	cleanupInstanceContainers(t, d, destinationID)

	// The large, incompressible world keeps the worker between server copy and container
	// creation long enough for the process kill to land on a durable boundary.
	awaitCheckpoint(t, db, accepted.JobID, "server_cloned")
	p.kill()

	dead, err := db.JobByID(t.Context(), accepted.JobID)
	if err != nil || dead == nil {
		t.Fatalf("read interrupted clone: %+v, %v", dead, err)
	}
	if dead.Status != "running" || dead.Checkpoint == nil || *dead.Checkpoint != "server_cloned" {
		t.Fatalf("kill missed the intended clone phase: %+v", dead)
	}
	destination, err := db.InstanceByID(t.Context(), destinationID)
	if err != nil || destination == nil {
		t.Fatalf("read destination after crash: %+v, %v", destination, err)
	}
	if destination.State != "provisioning" || destination.ContainerID != nil {
		t.Fatalf("destination at crash = %+v, want provisioning without a container", destination)
	}
	assertCloneLocks(t, db, sourceID, destinationID, true)
	if inspect(t, d, sourceContainerID).Running {
		t.Fatal("source server started before recovery")
	}

	p.restart()

	if job := p.awaitJob(accepted.JobID); job.Status != "failed" ||
		job.ErrorCode == nil || *job.ErrorCode != "interrupted" {
		t.Fatalf("recovered clone = %+v, want failed/interrupted", job)
	}
	if got := p.state(destinationID); got != "error" {
		t.Errorf("destination state = %q, want error", got)
	}
	if got := treeHash(t, destination.DataDir); got != serverBefore {
		t.Errorf("failed destination is not inspectable as the completed server copy:\n%s", got)
	}
	if got := p.state(sourceID); got != "stopped" {
		t.Errorf("source state = %q, want stopped", got)
	}
	assertCloneLocks(t, db, sourceID, destinationID, false)

	var cloneJobs int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'clone'`).Scan(&cloneJobs); err != nil {
		t.Fatal(err)
	}
	if cloneJobs != 1 {
		t.Errorf("clone jobs after recovery = %d, want only the interrupted job", cloneJobs)
	}

	sourceAfter, err := db.InstanceByID(t.Context(), sourceID)
	if err != nil || sourceAfter == nil {
		t.Fatalf("read source after recovery: %+v, %v", sourceAfter, err)
	}
	if !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Errorf("source row changed across clone crash:\nbefore: %+v\nafter:  %+v", sourceBefore, sourceAfter)
	}
	passwordAfter, err := db.InstancePassword(t.Context(), sourceID)
	if err != nil {
		t.Fatalf("read source password after recovery: %v", err)
	}
	if passwordAfter != passwordBefore {
		t.Error("source password envelope changed across clone crash")
	}
	if got := treeHash(t, sourceDir); got != serverBefore {
		t.Errorf("source server changed across clone crash:\nbefore:\n%s\n\nafter:\n%s", serverBefore, got)
	}
	assertCrashWorld(t, worldDir, worldBefore)

	if sourceContainer := inspect(t, d, sourceContainerID); sourceContainer.Running ||
		!sourceContainer.StartedAt.IsZero() {
		t.Errorf("source server ran during clone recovery: %+v", sourceContainer)
	}
	destinationContainers, err := d.List(t.Context(), map[string]string{
		instance.LabelManaged:    "true",
		instance.LabelInstanceID: destinationID,
	})
	if err != nil {
		t.Fatalf("list destination containers: %v", err)
	}
	if len(destinationContainers) != 0 {
		t.Errorf("clone recovery created destination containers: %+v", destinationContainers)
	}
}

func writeCloneSourceServer(t *testing.T, dataDir string) {
	t.Helper()
	files := map[string]string{
		"valheim_server.x86_64":                    "server binary\n",
		"steamapps/appmanifest_896660.acf":         `"AppState" { "appid" "896660" "buildid" "123456" }`,
		"BepInEx/config/operator-settings.cfg":     "enabled = true\n",
		"valheim_server_Data/StreamingAssets/data": "server asset\n",
	}
	for rel, body := range files {
		path := filepath.Join(dataDir, "server", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCloneLocks(t *testing.T, db *store.DB, sourceID, destinationID string, want bool) {
	t.Helper()
	held, err := db.HeldLockKeys(t.Context())
	if err != nil {
		t.Fatalf("read held job locks: %v", err)
	}
	for _, key := range []string{jobs.InstanceLockKey(sourceID), jobs.InstanceLockKey(destinationID)} {
		if held[key] != want {
			t.Errorf("lock %s held = %t, want %t", key, held[key], want)
		}
	}
}

func cleanupInstanceContainers(t *testing.T, d *runtime.Docker, instanceID string) {
	t.Helper()
	t.Cleanup(func() {
		containers, err := d.List(context.Background(), map[string]string{
			instance.LabelManaged:    "true",
			instance.LabelInstanceID: instanceID,
		})
		if err != nil {
			return
		}
		for i := range containers {
			_ = d.Remove(context.Background(), containers[i].ID, true)
		}
	})
}
