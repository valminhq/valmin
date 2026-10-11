package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

func updatePath(id string) string { return "/api/v1/instances/" + id + "/update" }

// makeModded plants the Doorstop library the modded test looks for (ADR-107).
func makeModded(t *testing.T, db *store.DB) {
	t.Helper()
	dir := filepath.Join(dataDirOf(t, db), "server", "doorstop_libs")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libdoorstop_x64.so"), []byte("doorstop"), 0o664); err != nil {
		t.Fatal(err)
	}
}

func postUpdate(t *testing.T, rt *Server, u *store.User, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, updatePath(id), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return as(rt, u, req)
}

// Asserts a modded instance is refused without explicit confirmation, and that the refusal
// creates no job row at all rather than one that fails a moment later (03 §8, ADR-137).
func TestUpdateOfAModdedInstanceNeedsConfirmation(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	makeModded(t, db)

	rec := postUpdate(t, rt, admin, seededInstanceID, `{}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed modded update = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "confirm_modded") {
		t.Errorf("the 422 does not name the field to set: %s", rec.Body)
	}
	if n := len(jobsOfKind(t, db, "game_update")); n != 0 {
		t.Errorf("%d game_update rows were created by a refused request, want 0", n)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want the instance untouched", got)
	}
}

// Asserts a vanilla instance needs no confirmation: the flag exists for the risk mods carry,
// not as a second click on every update.
func TestUpdateOfAVanillaInstanceNeedsNoConfirmation(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin

	rec := postUpdate(t, rt, admin, seededInstanceID, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update of a vanilla instance = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if n := len(jobsOfKind(t, db, "game_update")); n != 1 {
		t.Errorf("%d game_update rows, want 1", n)
	}
}

// Asserts the confirmation actually lets the update through: the flag is a question to answer,
// not a wall.
func TestAConfirmedModdedUpdateIsAccepted(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	makeModded(t, db)

	rec := postUpdate(t, rt, admin, seededInstanceID, `{"confirm_modded":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("confirmed modded update = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if n := len(jobsOfKind(t, db, "game_update")); n != 1 {
		t.Errorf("%d game_update rows, want 1", n)
	}
}

// Asserts an update of a running instance is refused and the server keeps running: replacing
// the files a live server has open is the one thing this job must never do (12 §3.1).
func TestUpdateOfARunningInstanceIsRefused(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin

	rec := postUpdate(t, rt, admin, seededInstanceID, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("update of a running instance = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "instance_must_be_stopped" {
		t.Errorf("code = %q, want instance_must_be_stopped", got)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %q, want running", got)
	}
	if n := len(jobsOfKind(t, db, "game_update")); n != 0 {
		t.Errorf("%d game_update rows were created, want 0", n)
	}
}

// Asserts instance.update is admin-only: it is never grantable, so even an operator with every
// grantable extra is refused (09 §3.3, ADR-137).
func TestUpdateIsAdminOnly(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, member := w.rt, w.db, w.member
	seed(t, db, `UPDATE instance_grants SET role = 'operator',
		perms = '["mods.manage","config.edit","config.raw","backups.restore","world.import","instance.settings"]'
		WHERE user_id = ?`, member.ID)

	if rec := postUpdate(t, rt, member, seededInstanceID, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("update as an operator holding every grantable = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec := postUpdate(t, rt, member, "inst-nope", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("update on an invisible instance = %d, want 404", rec.Code)
	}
}

// Asserts a schedule is standing permission and never standing confirmation: a tick against a
// modded instance records a skip and submits nothing (03 §8, ADR-137).
func TestAScheduledUpdateSkipsAModdedInstance(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	makeModded(t, db)
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "game_update", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("the tick produced %v, want one recorded skip", rows)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want the instance untouched", got)
	}
}

// Asserts a scheduled update of a vanilla instance runs, with no requester: the confirmation
// rule is about mods, and a vanilla server has nothing to confirm (ADR-137).
func TestAScheduledUpdateRunsOnAVanillaInstance(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "game_update", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Kind != "game_update" {
		t.Fatalf("the tick produced %v, want one game_update job", rows)
	}
	if rows[0].RequestedBy != nil {
		t.Errorf("requested_by = %v, want NULL for a scheduled run", *rows[0].RequestedBy)
	}
}

// Asserts an update that fails parks the instance in error and starts nothing (B7, ADR-137).
// The fake runtime has no SteamCMD, so the build fetch is what fails here — the earliest
// failure the job can have after it has claimed the instance.
func TestAFailedUpdateParksInErrorAndStartsNothing(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	// The throwaway container SteamCMD runs in exits non-zero, which is the earliest failure
	// the job can have once it has claimed the instance.
	w.fake.ExitCodes = []int{1}

	rec := postUpdate(t, rt, admin, seededInstanceID, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "failed" {
		t.Fatalf("update job = %+v, want failed", final)
	}

	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %q, want error (B7)", got)
	}
	for _, j := range jobsOfKind(t, db, "start") {
		t.Errorf("a start job was submitted after a failed update: %s", j)
	}
	// The pre-update archive is taken before anything else, so it is in the catalogue even
	// though the update itself got nowhere.
	if got := len(backupsWithTrigger(t, rt, admin, store.TriggerPreUpdate)); got != 1 {
		t.Errorf("%d pre_update archives in the catalogue, want 1", got)
	}
	// And nothing was staged: the tree the job would have replaced is untouched.
	if _, err := os.Stat(instance.StagedServerDir(dataDirOf(t, db))); !os.IsNotExist(err) {
		t.Error("a failed update left a staged server behind")
	}
}

// Asserts an update of a server Docker has running while its row says stopped fails with
// instance_must_be_stopped before it archives anything: the pre-update archive would be a copy
// of a live world catalogued as consistent.
func TestUpdateOfAServerRunningInDockerArchivesNothing(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, admin := w.rt, w.admin
	// Started behind the panel's back: the row still reads `stopped`.
	if err := w.fake.Start(t.Context(), w.containerID); err != nil {
		t.Fatal(err)
	}
	// Should the job get as far as the build fetch, it fails there rather than waiting on a
	// SteamCMD the fake does not have.
	w.fake.ExitCodes = []int{1}

	rec := postUpdate(t, rt, admin, seededInstanceID, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("update job = %+v, want failed", final)
	}
	if final.ErrorCode == nil || *final.ErrorCode != "instance_must_be_stopped" {
		t.Errorf("error_code = %q, want instance_must_be_stopped", deref(final.ErrorCode))
	}
	if got := len(backupsWithTrigger(t, rt, admin, store.TriggerPreUpdate)); got != 0 {
		t.Errorf("%d pre_update archives of a live world in the catalogue, want 0", got)
	}
}

// Asserts game_update is never resumed after a crash: auto-starting a server whose tree was
// being replaced is what B7 exists to forbid.
func TestGameUpdateIsNeverResumed(t *testing.T) {
	if jobs.ResumeIntentHonoured(jobs.KindGameUpdate) {
		t.Error("game_update is in resumeIntentHonoured; B7 forbids it")
	}
}
