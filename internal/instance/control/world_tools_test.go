package control

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// fakeSender records the commands sent and answers with err. A non-nil loaded refuses every
// send made before it is set.
type fakeSender struct {
	mu     sync.Mutex
	sent   []string
	err    error
	loaded *atomic.Bool
}

func (f *fakeSender) Send(_ context.Context, inst *store.Instance, raw string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst.State != string(instance.StateRunning) {
		return "", errors.New("sent to a server not presented as running")
	}
	if f.loaded != nil && !f.loaded.Load() {
		return "", errors.New("sent before the world loaded")
	}
	f.sent = append(f.sent, raw)
	if f.err != nil {
		return "", f.err
	}
	return "Command executed.", nil
}

// runWorldTool runs a world clean on inst-a against a server that is ready as soon as it starts.
func runWorldTool(t *testing.T, w *world, sender *fakeSender) *store.Job {
	t.Helper()
	w.c.Chainer.Commands = sender
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	return submitWorldTool(t, w)
}

// submitWorldTool submits a world clean on inst-a and waits for the job to end.
func submitWorldTool(t *testing.T, w *world) *store.Job {
	t.Helper()
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := w.c.WorldTooler.Submit(t.Context(), &WorldToolSubmission{
		Instance: inst, Tool: WorldTool{Tool: ToolUpgradeWorld, Action: ActionWorldClean},
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

// seedWorldFiles writes a world the pre-run backup can archive and verify.
func seedWorldFiles(t *testing.T, w *world) {
	t.Helper()
	dir := filepath.Join(instance.WorldsDir(instance.DataDir(w.cfg.Data.HostRoot, seededInstanceID)),
		instance.WorldsLocalDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := []byte("World")
	body := binary.LittleEndian.AppendUint32(nil, 37)
	body = append(append(body, byte(len(name))), name...)
	fwl := append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)
	files := map[string][]byte{
		"World.fwl": fwl,
		"World.db":  []byte(strings.Repeat("world save data ", 200)),
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), content, 0o644); err != nil {
			t.Fatal(err)
		}
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
	seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	sender := &fakeSender{}

	job := runWorldTool(t, w, sender)

	assertOutcome(t, w, job, jobs.StatusSucceeded, instance.StateRunning, 1)
	if job.Kind != jobs.KindWorldTool.String() {
		t.Errorf("job kind = %s, want %s", job.Kind, jobs.KindWorldTool)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "consoleCommand world_clean start" {
		t.Errorf("sent %q, want the world clean command once", sender.sent)
	}
}

// TestWorldToolWaitsForTheWorldToLoad asserts the command is sent only after the ready line,
// even when the start reported running on the settle fallback before it.
func TestWorldToolWaitsForTheWorldToLoad(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	var loaded atomic.Bool
	sender := &fakeSender{loaded: &loaded}
	w.c.Chainer.Commands = sender

	w.fake.OnStart = func(c *runtime.FakeContainer) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			loaded.Store(true)
			c.Stdout("Game server connected\n")
		}()
	}
	job := submitWorldTool(t, w)

	assertOutcome(t, w, job, jobs.StatusSucceeded, instance.StateRunning, 1)
	if len(sender.sent) != 1 {
		t.Errorf("sent %q, want the command once", sender.sent)
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

	job := runWorldTool(t, w, sender)

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
	seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	sender := &fakeSender{err: command.ErrUnsupported}

	job := runWorldTool(t, w, sender)

	assertOutcome(t, w, job, jobs.StatusFailed, instance.StateRunning, 1)
	if len(sender.sent) != 1 {
		t.Errorf("sent %d times, want one attempt for an error that cannot clear", len(sender.sent))
	}
}

// TestWorldToolDoesNotResendACommandThatMayHaveRun asserts a send that failed after the command
// was written is not sent again, since the server may already be running it.
func TestWorldToolDoesNotResendACommandThatMayHaveRun(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	seedWorldFiles(t, w)
	sender := &fakeSender{err: fmt.Errorf("read RCON command: %w", command.ErrNoReply)}

	job := runWorldTool(t, w, sender)

	assertOutcome(t, w, job, jobs.StatusFailed, instance.StateRunning, 1)
	if len(sender.sent) != 1 {
		t.Errorf("sent %d times, want one: a resend may run the command twice", len(sender.sent))
	}
}

// TestWorldToolRefusesARunningServer asserts a server that was started after the request was
// checked is not stopped for a backup and then changed live.
func TestWorldToolRefusesARunningServer(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateRunning))
	seedWorldFiles(t, w)
	sender := &fakeSender{}
	w.c.Chainer.Commands = sender

	job := submitWorldTool(t, w)

	assertOutcome(t, w, job, jobs.StatusFailed, instance.StateRunning, 0)
	if len(sender.sent) != 0 {
		t.Errorf("sent %q to a server that was running", sender.sent)
	}
}

// TestWorldToolWithoutAWorldSkipsTheBackup asserts a server with no worlds directory has nothing
// to archive and the tool still runs.
func TestWorldToolWithoutAWorldSkipsTheBackup(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	sender := &fakeSender{}

	job := runWorldTool(t, w, sender)

	assertOutcome(t, w, job, jobs.StatusSucceeded, instance.StateRunning, 0)
	if len(sender.sent) != 1 {
		t.Errorf("sent %q, want the command once", sender.sent)
	}
}

// TestWorldToolKeepsAWorldThatFailsVerification asserts the snapshot is taken as the world
// stands, since a world worth going back to may be one that would not verify.
func TestWorldToolKeepsAWorldThatFailsVerification(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	dir := filepath.Join(instance.WorldsDir(instance.DataDir(w.cfg.Data.HostRoot, seededInstanceID)),
		instance.WorldsLocalDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "World.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}

	job := runWorldTool(t, w, sender)

	assertOutcome(t, w, job, jobs.StatusSucceeded, instance.StateRunning, 1)
}
