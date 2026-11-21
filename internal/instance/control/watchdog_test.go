package control

import (
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
)

// TestHangWatchSilence asserts how long a server may stay quiet: fifteen minutes from its last
// status line, or twenty-five from its start when it has logged none.
func TestHangWatchSilence(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	beat := t0.Add(10 * time.Minute)
	tests := []struct {
		name  string
		pulse instance.Pulse
		now   time.Time
		want  bool
	}{
		{
			"just under fifteen minutes from the last line",
			instance.Pulse{Boot: t0, Beat: beat},
			beat.Add(15*time.Minute - time.Second), false,
		},
		{"fifteen minutes from the last line", instance.Pulse{Boot: t0, Beat: beat}, beat.Add(15 * time.Minute), true},
		{
			"just under twenty-five minutes from the start",
			instance.Pulse{Boot: t0},
			t0.Add(25*time.Minute - time.Second), false,
		},
		{"twenty-five minutes from the start", instance.Pulse{Boot: t0}, t0.Add(25 * time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h hangWatch
			if _, got := h.silent("a", tt.pulse, tt.now); got != tt.want {
				t.Errorf("silent = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHangWatchCountsSilenceFromWhenTheLogWasReadable asserts a silence is counted only from the
// moment the panel began reading the log again, since a line written while it could not read is
// one it never saw.
func TestHangWatchCountsSilenceFromWhenTheLogWasReadable(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	beat := t0.Add(10 * time.Minute)
	reopened := beat.Add(10 * time.Minute)
	pulse := instance.Pulse{Boot: t0, Beat: beat, Open: reopened}

	var h hangWatch
	if _, ok := h.silent("a", pulse, beat.Add(20*time.Minute)); ok {
		t.Error("silence from before the log was readable again was counted")
	}
	if _, ok := h.silent("a", pulse, reopened.Add(15*time.Minute)); !ok {
		t.Error("silence after the log was readable again was not counted")
	}
}

// TestHangWatchReportsOncePerEpisode asserts one silence is reported once, and a newer status
// line or a new boot makes the next silence a new episode.
func TestHangWatchReportsOncePerEpisode(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	var h hangWatch

	beat := t0.Add(10 * time.Minute)
	now := beat.Add(16 * time.Minute)
	anchor, ok := h.silent("a", instance.Pulse{Boot: t0, Beat: beat}, now)
	if !ok {
		t.Fatal("a silence past the limit was not reported")
	}
	h.markReported("a", anchor)
	if _, ok := h.silent("a", instance.Pulse{Boot: t0, Beat: beat}, now.Add(time.Minute)); ok {
		t.Error("the same silence was reported twice")
	}

	next := now.Add(time.Minute)
	later, ok := h.silent("a", instance.Pulse{Boot: t0, Beat: next.Add(-16 * time.Minute)}, next)
	if !ok {
		t.Error("a silence after a newer status line was not a new episode")
	}
	h.markReported("a", later)

	rebooted := next.Add(time.Hour)
	if _, ok := h.silent("a", instance.Pulse{Boot: rebooted.Add(-26 * time.Minute)}, rebooted); !ok {
		t.Error("a silence after a new boot was not a new episode")
	}
	if _, ok := h.silent("b", instance.Pulse{Boot: t0, Beat: beat}, now); !ok {
		t.Error("another server's report was held back by the first")
	}
}

// TestHangWatchIgnoresASuspend asserts a gap between passes, a suspended host or daemon, does not
// count as the server's silence: the clock restarts from the pass that ended the gap.
func TestHangWatchIgnoresASuspend(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	var h hangWatch
	h.tick(t0)
	if gap := h.tick(t0.Add(observeInterval)); gap != 0 {
		t.Errorf("a normal pass reported a gap of %v", gap)
	}

	woke := t0.Add(2 * time.Hour)
	if gap := h.tick(woke); gap < time.Hour {
		t.Errorf("the pass after a suspend reported a gap of %v, want about two hours", gap)
	}
	pulse := instance.Pulse{Boot: t0, Beat: t0}
	if _, ok := h.silent("a", pulse, woke.Add(10*time.Minute)); ok {
		t.Error("silence was counted across the suspend")
	}
	if _, ok := h.silent("a", pulse, woke.Add(15*time.Minute)); !ok {
		t.Error("silence after the suspend was not counted")
	}
}

// TestHangWatchToleratesASlowPass asserts a pass that runs somewhat late is not mistaken for a
// suspend, since a live server is never quiet for more than ten minutes of the fifteen allowed.
func TestHangWatchToleratesASlowPass(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	var h hangWatch
	h.tick(t0)
	if gap := h.tick(t0.Add(90 * time.Second)); gap != 0 {
		t.Errorf("a pass 90 seconds after the last was taken for a suspend of %v", gap)
	}
}

// TestHangWatchStartsEveryServerFromTheFirstPass asserts the first pass after the daemon starts
// counts no silence from before it, since the logs may not have been read up to date yet.
func TestHangWatchStartsEveryServerFromTheFirstPass(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	stale := instance.Pulse{Boot: t0.Add(-time.Hour), Beat: t0.Add(-time.Hour)}
	var h hangWatch
	h.tick(t0)
	if _, ok := h.silent("a", stale, t0.Add(10*time.Minute)); ok {
		t.Error("silence from before the daemon started was counted")
	}
	if _, ok := h.silent("a", stale, t0.Add(15*time.Minute)); !ok {
		t.Error("silence after the first pass was not counted")
	}
}

// TestHangWatchDoesNotRepeatAReportAfterARearm asserts a server already reported as hung is not
// reported again because a later pass was late: a rearm delays a report, it does not repeat one.
func TestHangWatchDoesNotRepeatAReportAfterARearm(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	pulse := instance.Pulse{Boot: t0, Beat: t0}
	var h hangWatch
	h.tick(t0)
	anchor, ok := h.silent("a", pulse, t0.Add(16*time.Minute))
	if !ok {
		t.Fatal("a silence past the limit was not reported")
	}
	h.markReported("a", anchor)

	late := t0.Add(2 * time.Hour)
	h.tick(late)
	if _, ok := h.silent("a", pulse, late.Add(time.Hour)); ok {
		t.Error("a rearm made a new episode of a silence already reported")
	}
}

// TestHangWatchWorksInWallClockTime asserts the passes are recorded without Go's monotonic
// reading, which stops while the host is suspended and so would hide a suspend from the gap check.
func TestHangWatchWorksInWallClockTime(t *testing.T) {
	var h hangWatch
	h.tick(time.Now())
	if strings.Contains(h.lastTick.String(), "m=") || strings.Contains(h.rearmed.String(), "m=") {
		t.Errorf("a pass kept its monotonic reading: %v, %v", h.lastTick, h.rearmed)
	}
}

// TestHangWatchRestartsOncePerHour asserts the watchdog restarts a server at most once an hour.
func TestHangWatchRestartsOncePerHour(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	var h hangWatch
	if !h.mayRestart("a", t0) {
		t.Fatal("a server never restarted was refused")
	}
	h.markRestarted("a", t0)
	if h.mayRestart("a", t0.Add(59*time.Minute)) {
		t.Error("a second restart inside the hour was allowed")
	}
	if !h.mayRestart("a", t0.Add(time.Hour)) {
		t.Error("a restart after the hour was refused")
	}
	if !h.mayRestart("b", t0.Add(time.Minute)) {
		t.Error("another server's restart was held back by the first")
	}
}

// TestHangWatchStillOwesARestartTheCooldownHeldBack asserts a server that hung again inside the
// hour after a restart is reported then, and restarted once the hour is up, not left hung for good.
func TestHangWatchStillOwesARestartTheCooldownHeldBack(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	pulse := instance.Pulse{Boot: t0, Beat: t0.Add(10 * time.Minute)}
	var h hangWatch
	h.tick(t0)
	h.markRestarted("a", t0)

	hungAt := t0.Add(30 * time.Minute)
	anchor, ok := h.silent("a", pulse, hungAt)
	if !ok {
		t.Fatal("the second hang was not reported")
	}
	h.markReported("a", anchor)
	h.markOwed("a", anchor)

	if _, ok := h.silent("a", pulse, hungAt.Add(10*time.Minute)); ok {
		t.Error("the restart was tried again before the hour was up")
	}
	again := t0.Add(61 * time.Minute)
	if _, ok := h.silent("a", pulse, again); !ok {
		t.Error("the restart the cooldown held back was never tried once the hour was up")
	}
	h.markRestarted("a", again)
	if _, ok := h.silent("a", pulse, again.Add(time.Minute)); ok {
		t.Error("the owed restart was still owed after it was made")
	}
}

// watchdogServer seeds a running server with the given watchdog mode, whose reader has looked at
// its boot and is following its log but whose log has no status line yet. It returns the world,
// the notifier, the container, and when the boot began.
func watchdogServer(t *testing.T, mode instance.Watchdog) (*world, *recordingNotifier, string, time.Time) {
	t.Helper()
	w := newWorld(t)
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	notifier := &recordingNotifier{}
	w.c.Supervisor.Notifier = notifier
	containerID := seedInstance(t, w, string(instance.StateRunning))
	seed(t, w.db, `UPDATE instances SET watchdog = ? WHERE id = ?`, string(mode), seededInstanceID)
	if err := w.c.Supervisor.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := w.c.Supervisor.Streams.Reader(seededInstanceID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if p := r.Pulse(); !p.Boot.IsZero() && !p.Open.IsZero() {
			return w, notifier, containerID, p.Boot
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader never opened the log")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// watchdogWorld is watchdogServer after the server has logged one status line, and returns the
// world, the notifier and the receive time of that line.
func watchdogWorld(t *testing.T, mode instance.Watchdog) (*world, *recordingNotifier, time.Time) {
	t.Helper()
	w, notifier, containerID, _ := watchdogServer(t, mode)
	w.fake.Get(containerID).Stdout("11/13/2026 19:57:00:  Connections 0 ZDOS:82  sent:0 recv:0\n")
	r := w.c.Supervisor.Streams.Reader(seededInstanceID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if p := r.Pulse(); !p.Beat.IsZero() {
			return w, notifier, p.Beat
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader never recorded the status line")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stepWatchdog runs the watchdog every observe interval from one instant to another, as the
// supervisor's loop would.
func stepWatchdog(t *testing.T, s *Supervisor, from, to time.Time) {
	t.Helper()
	for at := from; !at.After(to); at = at.Add(observeInterval) {
		s.watchdog(t.Context(), at)
	}
}

// countJobs counts the jobs of kind submitted for the seeded instance.
func countJobs(t *testing.T, w *world, kind jobs.Kind) int {
	t.Helper()
	var n int
	if err := w.db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = ? AND instance_id = ?`,
		kind.String(), seededInstanceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWatchdogAlertsOnceAndLeavesTheServerRunning asserts that in alert mode a server quiet for
// fifteen minutes is reported once, as still running, and no job touches it.
func TestWatchdogAlertsOnceAndLeavesTheServerRunning(t *testing.T) {
	w, notifier, beat := watchdogWorld(t, instance.WatchdogAlert)
	sup := w.c.Supervisor

	stepWatchdog(t, sup, beat, beat.Add(14*time.Minute))
	if len(notifier.downs) != 0 {
		t.Fatalf("notified after 14 minutes: %v", notifier.downs)
	}
	stepWatchdog(t, sup, beat.Add(14*time.Minute+observeInterval), beat.Add(40*time.Minute))
	want := string(instance.StateRunning) + ": " + instance.ReasonUnresponsive
	if len(notifier.downs) != 1 || notifier.downs[0] != want {
		t.Fatalf("notifications = %q, want exactly %q", notifier.downs, want)
	}
	if got := incidentCount(t, w.db); got != 1 {
		t.Errorf("incidents = %d, want 1", got)
	}
	if n := countJobs(t, w, jobs.KindRestart) + countJobs(t, w, jobs.KindStop); n != 0 {
		t.Errorf("%d jobs touched a server the watchdog may only report", n)
	}
	if got := stateOf(t, w.db); got != string(instance.StateRunning) {
		t.Errorf("state = %s, want running", got)
	}
}

// TestWatchdogDoesNothingWhenOffOrNotRunning asserts a quiet server is left alone when its
// watchdog is off, and that a stopped server is never reported.
func TestWatchdogDoesNothingWhenOffOrNotRunning(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		w, notifier, beat := watchdogWorld(t, instance.WatchdogOff)
		stepWatchdog(t, w.c.Supervisor, beat, beat.Add(40*time.Minute))
		if len(notifier.downs) != 0 {
			t.Errorf("notified with the watchdog off: %v", notifier.downs)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		w, notifier, beat := watchdogWorld(t, instance.WatchdogAlert)
		seed(t, w.db, `UPDATE instances SET state = 'stopped' WHERE id = ?`, seededInstanceID)
		stepWatchdog(t, w.c.Supervisor, beat, beat.Add(40*time.Minute))
		if len(notifier.downs) != 0 {
			t.Errorf("notified for a stopped server: %v", notifier.downs)
		}
	})
}

// TestWatchdogNeverRestartsAServerThatHasLoggedNoStatusLine asserts a server quiet since its boot
// began is reported but not restarted even in restart mode, because it may be loading its world.
func TestWatchdogNeverRestartsAServerThatHasLoggedNoStatusLine(t *testing.T) {
	w, notifier, _, boot := watchdogServer(t, instance.WatchdogRestart)
	stepWatchdog(t, w.c.Supervisor, boot, boot.Add(26*time.Minute))

	want := string(instance.StateRunning) + ": " + instance.ReasonUnresponsive
	if len(notifier.downs) != 1 || notifier.downs[0] != want {
		t.Fatalf("notifications = %q, want exactly %q", notifier.downs, want)
	}
	if got := countJobs(t, w, jobs.KindRestart); got != 0 {
		t.Errorf("restart jobs = %d for a server that never logged a status line, want 0", got)
	}
}

// TestWatchdogRestartsAHungServer asserts that in restart mode a quiet server is reported at once
// even while a job holds it, and restarted, with a second report, as soon as the job lets go.
func TestWatchdogRestartsAHungServer(t *testing.T) {
	w, notifier, beat := watchdogWorld(t, instance.WatchdogRestart)
	sup := w.c.Supervisor
	quiet := beat.Add(16 * time.Minute)

	held := seedStaleJob(t, w.db, jobs.KindBackup.String(), "", "{}")
	stepWatchdog(t, sup, beat, quiet.Add(time.Minute))
	reported := string(instance.StateRunning) + ": " + instance.ReasonUnresponsive
	if len(notifier.downs) != 1 || notifier.downs[0] != reported {
		t.Fatalf("a held server was reported as %q, want exactly %q", notifier.downs, reported)
	}
	if got := countJobs(t, w, jobs.KindRestart); got != 0 {
		t.Fatalf("a held server was restarted: %d restart jobs", got)
	}

	seed(t, w.db, `DELETE FROM job_locks WHERE job_id = ?`, held)
	seed(t, w.db, `DELETE FROM job_runs WHERE id = ?`, held)
	// One pass, not a run of them: the restart starts a new boot in real time, which a clock
	// already past it would count as quiet.
	sup.watchdog(t.Context(), quiet.Add(time.Minute+observeInterval))
	restarting := string(instance.StateRunning) + ": " + instance.ReasonUnresponsiveRestarting
	if len(notifier.downs) != 2 || notifier.downs[1] != restarting {
		t.Fatalf("notifications = %q, want the restart reported second as %q", notifier.downs, restarting)
	}
	if got := countJobs(t, w, jobs.KindRestart); got != 1 {
		t.Errorf("restart jobs = %d, want 1", got)
	}
}

// TestWatchdogDoesNotActOnASilenceTheLogContradicts asserts that a status line in the container's
// log that the reader never recorded stops the report: a follow stream that stalled would
// otherwise make a healthy server look hung.
func TestWatchdogDoesNotActOnASilenceTheLogContradicts(t *testing.T) {
	for _, mode := range []instance.Watchdog{instance.WatchdogAlert, instance.WatchdogRestart} {
		t.Run(string(mode), func(t *testing.T) {
			w, notifier, beat := watchdogWorld(t, mode)
			inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			sup := w.c.Supervisor
			now := beat.Add(16 * time.Minute)

			// The reader believes the silence began long before the line in the log.
			sup.handleHang(t.Context(), inst,
				hungServer{mode: mode, seen: true, anchor: beat.Add(-time.Hour)}, now)
			if len(notifier.downs) != 0 || countJobs(t, w, jobs.KindRestart) != 0 {
				t.Fatalf("acted on a silence the log contradicts: %q, %d restarts",
					notifier.downs, countJobs(t, w, jobs.KindRestart))
			}

			// With nothing newer in the log the silence stands.
			sup.handleHang(t.Context(), inst,
				hungServer{mode: mode, seen: true, anchor: beat.Add(time.Hour)}, now)
			if len(notifier.downs) != 1 {
				t.Errorf("notifications = %q for a silence the log confirms, want one", notifier.downs)
			}
		})
	}
}
