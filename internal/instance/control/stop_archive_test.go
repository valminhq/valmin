package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// stopArchiveWorld seeds a running server with a world on disk whose container is ready on
// start. A stop of it logs the save-complete line only when saves is set, and the instance
// archives on stops only when archiving is set.
func stopArchiveWorld(t *testing.T, saves, archiving bool) *world {
	t.Helper()
	w := newWorld(t)
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	if saves {
		w.fake.OnStop = func(c *runtime.FakeContainer) {
			c.Stdout("World save writing starting\n")
			c.Stdout("World save writing finished\n")
		}
	}
	seedInstance(t, w, string(instance.StateRunning))
	seedWorldFiles(t, w)
	if archiving {
		seed(t, w.db, `UPDATE instances SET backup_on_restart = TRUE WHERE id = ?`, seededInstanceID)
	}
	return w
}

// submitStop queues a stop of the seeded server and waits for its job.
func submitStop(t *testing.T, w *world, submit func(*store.Instance) (*store.Job, error)) *store.Job {
	t.Helper()
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := submit(inst)
	if err != nil {
		t.Fatalf("submit stop: %v", err)
	}
	return awaitJob(t, w, job.ID)
}

// stopOf queues a stop with the given Archive flag.
func stopOf(w *world, archive bool) func(*store.Instance) (*store.Job, error) {
	return func(inst *store.Instance) (*store.Job, error) {
		return w.c.Stopper.Submit(context.Background(), &StopSubmission{
			Instance: inst, ContainerID: deref(inst.ContainerID), Archive: archive,
		})
	}
}

// TestStopArchivesOnlyWhenAskedAndConfirmed asserts a stop writes one consistent archive only
// when the instance opted in, the stop asked for one and the save was confirmed.
func TestStopArchivesOnlyWhenAskedAndConfirmed(t *testing.T) {
	tests := []struct {
		name      string
		saves     bool
		archiving bool
		archive   bool
		want      int
	}{
		{"opted in, asked and confirmed", true, true, true, 1},
		{"opted in and asked, save not confirmed", false, true, true, 0},
		{"opted in, a person's stop", true, true, false, 0},
		{"asked but not opted in", true, false, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := stopArchiveWorld(t, tt.saves, tt.archiving)
			job := submitStop(t, w, stopOf(w, tt.archive))

			if job.Status != jobs.StatusSucceeded {
				t.Fatalf("stop status = %s (%v), want succeeded", job.Status, deref(job.Error))
			}
			if got := instanceState(t, w); got != instance.StateStopped {
				t.Errorf("state = %s, want stopped", got)
			}
			if job.Clean == nil || *job.Clean != tt.saves {
				t.Errorf("clean = %v, want %v", job.Clean, tt.saves)
			}
			rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tt.want {
				t.Fatalf("%d backups, want %d", len(rows), tt.want)
			}
			if tt.want == 1 && (!rows[0].Consistent || rows[0].Trigger != store.TriggerManual) {
				t.Errorf("backup = consistent %v trigger %q, want consistent manual",
					rows[0].Consistent, rows[0].Trigger)
			}

			// The job names the file it may write when it is queued, so a crash can be cleaned up.
			var plan ArchivePlan
			if err := json.Unmarshal([]byte(job.Payload), &plan); err != nil {
				t.Fatalf("read the stop's payload %q: %v", job.Payload, err)
			}
			if planned := plan.ArchivePath != ""; planned != (tt.archiving && tt.archive) {
				t.Errorf(
					"payload plans an archive = %v, want %v (%s)",
					planned,
					tt.archiving && tt.archive,
					job.Payload,
				)
			}
			if tt.want == 1 && (plan.ArchiveID != rows[0].ID || plan.ArchivePath != rows[0].Path) {
				t.Errorf("payload plan = %+v, want the archive %s at %s", plan, rows[0].ID, rows[0].Path)
			}
		})
	}
}

// TestAStopFinishesWhenItsArchiveCannotBeRecorded asserts a catalogue that refuses the archive's
// row never keeps a stop or restart from finishing: the server still ends up stopped or running,
// no row is left behind, and the file nothing names is removed.
func TestAStopFinishesWhenItsArchiveCannotBeRecorded(t *testing.T) {
	tests := []struct {
		name      string
		submit    func(*world) func(*store.Instance) (*store.Job, error)
		wantState instance.State
	}{
		{
			"stop",
			func(w *world) func(*store.Instance) (*store.Job, error) { return stopOf(w, true) },
			instance.StateStopped,
		},
		{
			"restart",
			func(w *world) func(*store.Instance) (*store.Job, error) {
				return func(inst *store.Instance) (*store.Job, error) {
					return w.c.Restarter.Submit(context.Background(), &RestartSubmission{
						Instance: inst, ContainerID: deref(inst.ContainerID),
					})
				}
			},
			instance.StateRunning,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := stopArchiveWorld(t, true, true)
			seed(t, w.db, `CREATE TRIGGER refuse_backup BEFORE INSERT ON backups
				BEGIN SELECT RAISE(ABORT, 'the catalogue is full'); END`)
			job := submitStop(t, w, tt.submit(w))

			if job.Status != jobs.StatusSucceeded {
				t.Fatalf("status = %s (%v), want succeeded", job.Status, deref(job.Error))
			}
			if got := instanceState(t, w); got != tt.wantState {
				t.Errorf("state = %s, want %s", got, tt.wantState)
			}
			rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("%d backups recorded, want none", len(rows))
			}

			var plan ArchivePlan
			if err := json.Unmarshal([]byte(job.Payload), &plan); err != nil || plan.ArchivePath == "" {
				t.Fatalf("the job planned no archive: %q (%v)", job.Payload, err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for exists(plan.ArchivePath) {
				if time.Now().After(deadline) {
					t.Fatalf("the archive %s no row names was left on disk", plan.ArchivePath)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestChainStopArchivesOnlyWhenScheduled asserts the stop step of a schedule archives and the
// same step run for anyone else does not.
func TestChainStopArchivesOnlyWhenScheduled(t *testing.T) {
	tests := []struct {
		name     string
		schedule bool
		want     int
	}{
		{"fired by a schedule", true, 1},
		{"not fired by a schedule", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := stopArchiveWorld(t, true, true)
			var origin StepOrigin
			if tt.schedule {
				seed(t, w.db, `INSERT INTO scheduled_jobs (id, instance_id, kind, cron)
					VALUES ('sched-a', ?, 'chain', '0 4 * * *')`, seededInstanceID)
				origin = StepOrigin{ScheduleID: "sched-a"}
			}
			job := submitStop(t, w, func(inst *store.Instance) (*store.Job, error) {
				return w.c.Chainer.SubmitStep(t.Context(), inst, &ChainStep{Type: StepStop}, origin)
			})

			if job.Status != jobs.StatusSucceeded {
				t.Fatalf("stop status = %s (%v), want succeeded", job.Status, deref(job.Error))
			}
			rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tt.want {
				t.Fatalf("%d backups, want %d", len(rows), tt.want)
			}
		})
	}
}

// TestAutoStopArchives asserts the stop auto-stop queues archives when the instance opted in.
func TestAutoStopArchives(t *testing.T) {
	w := stopArchiveWorld(t, true, true)
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	inst.AutoStopMinutes = 5
	w.c.Supervisor.submitAutoStop(t.Context(), inst)

	var id string
	if err := w.db.Reader.QueryRowContext(t.Context(),
		`SELECT id FROM job_runs WHERE kind = ? AND instance_id = ?`,
		jobs.KindStop.String(), seededInstanceID).Scan(&id); err != nil {
		t.Fatalf("find the auto-stop job: %v", err)
	}
	if job := awaitJob(t, w, id); job.Status != jobs.StatusSucceeded {
		t.Fatalf("stop status = %s (%v), want succeeded", job.Status, deref(job.Error))
	}
	rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d backups after an auto-stop, want 1", len(rows))
	}
}

// TestStopSkipsTheArchiveWithoutRoom asserts a stop still completes, and takes no archive and
// says why, when the data root is below the free-space floor or its free space cannot be read.
func TestStopSkipsTheArchiveWithoutRoom(t *testing.T) {
	tests := []struct {
		name    string
		free    func() (uint64, error)
		want    int
		wantLog string
	}{
		{"below the floor", func() (uint64, error) { return 1 << 20, nil }, 0, "below the floor"},
		{"unreadable", func() (uint64, error) { return 0, errors.New("statfs failed") }, 0, "statfs failed"},
		{"room to spare", func() (uint64, error) { return 3 << 30, nil }, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := stopArchiveWorld(t, true, true)
			w.c.Backupper.FreeSpace, w.c.Backupper.FreeSpaceFloor = tt.free, 2<<30
			job := submitStop(t, w, stopOf(w, true))

			if job.Status != jobs.StatusSucceeded {
				t.Fatalf("stop status = %s (%v), want succeeded", job.Status, deref(job.Error))
			}
			if got := instanceState(t, w); got != instance.StateStopped {
				t.Errorf("state = %s, want stopped", got)
			}
			rows, err := w.db.ListBackups(t.Context(), seededInstanceID, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tt.want {
				t.Fatalf("%d backups, want %d", len(rows), tt.want)
			}
			if got := deref(job.Log); !strings.Contains(got, tt.wantLog) {
				t.Errorf("job log = %q, want it to mention %q", got, tt.wantLog)
			}
		})
	}
}
