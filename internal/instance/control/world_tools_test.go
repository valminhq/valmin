package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// TestWorldToolCommand asserts the console line each action sends and the requests refused.
func TestWorldToolCommand(t *testing.T) {
	tests := []struct {
		name string
		tool WorldTool
		want string
	}{
		{
			"zones reset",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionZonesReset},
			"consoleCommand zones_reset start",
		},
		{
			"zones reset beyond a distance",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionZonesReset, MinDistanceM: 5000},
			"consoleCommand zones_reset start min=5000",
		},
		{
			"upgrade",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionUpgrade, Operation: "tarpits"},
			"consoleCommand upgrade tarpits start",
		},
		{
			"world clean",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionWorldClean},
			"consoleCommand world_clean start",
		},
		{"fresh world", WorldTool{Tool: ToolFreshWorld, Action: ActionRun}, "consoleCommand freshworld"},
		{
			"worldgen upgrade",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionUpgrade, Operation: "mistlands_worldgen"},
			"",
		},
		{
			"operation with arguments",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionUpgrade, Operation: "tarpits force"},
			"",
		},
		{"empty operation", WorldTool{Tool: ToolUpgradeWorld, Action: ActionUpgrade}, ""},
		{
			"operation on another action",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionWorldClean, Operation: "tarpits"},
			"",
		},
		{
			"distance on another action",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionWorldClean, MinDistanceM: 10},
			"",
		},
		{"negative distance", WorldTool{Tool: ToolUpgradeWorld, Action: ActionZonesReset, MinDistanceM: -1}, ""},
		{
			"distance past the edge",
			WorldTool{Tool: ToolUpgradeWorld, Action: ActionZonesReset, MinDistanceM: 20001},
			"",
		},
		{"action of the other tool", WorldTool{Tool: ToolFreshWorld, Action: ActionWorldClean}, ""},
		{"unknown tool", WorldTool{Tool: "other", Action: ActionRun}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.tool.Command()
			if tt.want == "" {
				if !errors.Is(err, ErrInvalidWorldTool) {
					t.Fatalf("Command() = %q, %v; want ErrInvalidWorldTool", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Command() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// fakeSender records the commands sent and answers with err.
type fakeSender struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (f *fakeSender) Send(_ context.Context, inst *store.Instance, raw string, unrestricted bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst.State != string(instance.StateRunning) || !unrestricted {
		return "", errors.New("sent to a server not presented as running, or restricted")
	}
	f.sent = append(f.sent, raw)
	if f.err != nil {
		return "", f.err
	}
	return "Command executed.", nil
}

// runWorldTool submits a world clean on inst-a and waits for the job to end.
func runWorldTool(t *testing.T, w *world, containerID string, sender *fakeSender) *store.Job {
	t.Helper()
	w.c.WorldTooler.Commands = sender
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := w.c.WorldTooler.Submit(t.Context(), &WorldToolSubmission{
		Instance: inst, ContainerID: containerID,
		Tool: WorldTool{Tool: ToolUpgradeWorld, Action: ActionWorldClean},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := w.db.JobByID(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "queued" && got.Status != "running" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("world tool job did not finish")
	return nil
}

// seedWorldFiles writes a world for the pre-run backup to archive.
func seedWorldFiles(t *testing.T, w *world) {
	t.Helper()
	dir := filepath.Join(instance.WorldsDir(instance.DataDir(w.cfg.Data.HostRoot, seededInstanceID)),
		instance.WorldsLocalDir, "World")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_main.1.db2"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertOutcome checks the job status, the instance state and how many pre-run backups exist.
func assertOutcome(t *testing.T, w *world, job *store.Job, status string, state instance.State, backups int) {
	t.Helper()
	if job.Status != status {
		t.Errorf("job status = %s (%v), want %s", job.Status, deref(job.Error), status)
	}
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != string(state) {
		t.Errorf("instance state = %s, want %s", inst.State, state)
	}
	rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != backups {
		t.Fatalf("%d backups recorded, want %d", len(rows), backups)
	}
	for i := range rows {
		if b := &rows[i]; b.Trigger != store.TriggerPreUpdate {
			t.Errorf("backup trigger = %s, want %s", b.Trigger, store.TriggerPreUpdate)
		}
	}
}

// TestWorldToolBacksUpStartsAndSends asserts the world is archived, the server started and the
// command sent, leaving the instance running.
func TestWorldToolBacksUpStartsAndSends(t *testing.T) {
	w := newWorld(t)
	containerID := seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	sender := &fakeSender{}

	job := runWorldTool(t, w, containerID, sender)

	assertOutcome(t, w, job, jobs.StatusSucceeded, instance.StateRunning, 1)
	if len(sender.sent) != 1 || sender.sent[0] != "consoleCommand world_clean start" {
		t.Errorf("sent %q, want the world clean command once", sender.sent)
	}
}

// TestWorldToolLeavesTheServerStoppedWhenTheBackupFails asserts nothing is started or sent when
// the backup cannot be taken.
func TestWorldToolLeavesTheServerStoppedWhenTheBackupFails(t *testing.T) {
	w := newWorld(t)
	containerID := seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	// The row says stopped but the container runs, so the backup's stop check refuses.
	if err := w.fake.Start(t.Context(), containerID); err != nil {
		t.Fatal(err)
	}
	runs := w.fake.Runs()
	sender := &fakeSender{}

	job := runWorldTool(t, w, containerID, sender)

	assertOutcome(t, w, job, jobs.StatusFailed, instance.StateStopped, 0)
	if len(sender.sent) != 0 {
		t.Errorf("sent %q after a failed backup", sender.sent)
	}
	if w.fake.Runs() != runs {
		t.Error("the server was started after a failed backup")
	}
}

// TestWorldToolKeepsTheBackupWhenTheSendFails asserts a refused send fails the job without
// retrying, while the backup stays recorded and the instance ends running.
func TestWorldToolKeepsTheBackupWhenTheSendFails(t *testing.T) {
	w := newWorld(t)
	containerID := seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	sender := &fakeSender{err: command.ErrUnsupported}

	job := runWorldTool(t, w, containerID, sender)

	assertOutcome(t, w, job, jobs.StatusFailed, instance.StateRunning, 1)
	if len(sender.sent) != 1 {
		t.Errorf("sent %d times, want one attempt for an error that cannot clear", len(sender.sent))
	}
}
