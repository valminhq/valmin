package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

const updateStatusPath = "/api/v1/instances/inst-a/update-status"

func steamReply(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../instance/testdata/steam/app-info.vdf")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readUpdateStatus(t *testing.T, rt *Router, u *store.User) updateStatusView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, updateStatusPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status: %d %s", rec.Code, rec.Body)
	}
	var v updateStatusView
	decodeInto(t, rec, &v)
	return v
}

func TestUpdateStatusComparesKnownBuilds(t *testing.T) {
	for _, tc := range []struct {
		name, installed  string
		manifest         bool
		known, available bool
	}{
		{"same", "21981590", false, true, false},
		{"different", "123", false, true, true},
		{"legacy", "latest", true, true, false},
		{"missing manifest", "latest", false, false, false},
		{"invalid stored id", "broken", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, fake, admin, _ := lifecycleWorld(t)
			seedInstance(t, rt, db, fake, "stopped")
			seed(t, db, `UPDATE instances SET game_build_id = ? WHERE id = 'inst-a'`, tc.installed)
			if tc.manifest {
				inst, _ := db.InstanceByID(t.Context(), seededInstanceID)
				dir := filepath.Join(inst.DataDir, "server", "steamapps")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile("../instance/testdata/steam/appmanifest.acf")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "appmanifest_896660.acf"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if v := readUpdateStatus(t, rt, admin); v.UpdateAvailable != nil || v.PublicBuildID != nil {
				t.Fatal("unobserved public build reported known")
			}
			stamp := time.Now().UTC().Add(-time.Hour)
			if err := db.KVSet(
				t.Context(),
				publicBuildKey,
				publicBuild{BuildID: "21981590", ObservedAt: stamp},
			); err != nil {
				t.Fatal(err)
			}
			v := readUpdateStatus(t, rt, admin)
			if (v.UpdateAvailable != nil) != tc.known {
				t.Fatalf("availability = %v, want known=%v", v.UpdateAvailable, tc.known)
			}
			if tc.known && *v.UpdateAvailable != tc.available {
				t.Errorf("available=%v, want %v", *v.UpdateAvailable, tc.available)
			}
			if v.ObservedAt == nil || !v.ObservedAt.Equal(stamp) {
				t.Errorf("observation timestamp = %v", v.ObservedAt)
			}
		})
	}
}

func TestUpdateCheckScheduleAndVisibility(t *testing.T) {
	rt, db, fake, admin, member := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")
	inst, _ := db.InstanceByID(t.Context(), seededInstanceID)
	dir := filepath.Join(inst.DataDir, "server", "doorstop_libs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libdoorstop_x64.so"), []byte("modded"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply := steamReply(t)
	specs := make(chan runtime.ContainerSpec, 1)
	fake.OnStart = func(c *runtime.FakeContainer) { specs <- c.Spec; c.Stdout(reply); c.Exit(0) }
	scheduleID := seedScheduleRow(t, db, "update_check", nil, time.Now().Add(-time.Minute))
	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now())
	var id string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT id FROM job_runs WHERE schedule_id=?`, scheduleID).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, id); got.Status != "succeeded" {
		t.Fatalf("job = %+v", got)
	}
	spec := <-specs
	if spec.User != "10000:10000" || len(spec.Binds) != 0 ||
		strings.Join(spec.Cmd, " ") != "+login anonymous +app_info_print 896660 +quit" {
		t.Fatalf("unsafe query spec: %+v", spec)
	}
	if len(spec.Env) != 1 || spec.Env[0] != "HOME=/tmp" {
		t.Fatalf("env=%v", spec.Env)
	}
	j := jobRow(t, db, id)
	if j.RequestedBy != nil || j.InstanceID != nil {
		t.Fatal("check attributed to a user or instance")
	}
	var count int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT count(*) FROM job_runs WHERE kind <> 'update_check'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || stateOf(t, db) != "running" {
		t.Fatal("check applied a change to the game")
	}
	c, err := fake.Inspect(t.Context(), containerID)
	if err != nil || !c.Running {
		t.Fatalf("game interrupted: %+v %v", c, err)
	}
	all, err := fake.List(t.Context(), nil)
	if err != nil || len(all) != 1 {
		t.Fatalf("throwaway leaked: %d %v", len(all), err)
	}
	seed(t, db, `DELETE FROM instance_grants WHERE user_id=? AND instance_id=?`, member.ID, seededInstanceID)
	if rec := as(rt, member, httptest.NewRequest(http.MethodGet, updateStatusPath, http.NoBody)); rec.Code != 404 {
		t.Fatalf("invisible status=%d", rec.Code)
	}
	if err := db.CreateGrant(
		t.Context(),
		member.ID,
		seededInstanceID,
		store.GrantViewer,
		"[]",
		admin.ID,
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if v := readUpdateStatus(t, rt, member); v.PublicBuildID == nil || *v.PublicBuildID != "21981590" {
		t.Fatalf("status=%+v", v)
	}
	if rec := postSchedule(
		t,
		rt,
		member,
		`{"kind":"update_check","cron":"@hourly"}`,
	); rec.Code != 404 &&
		rec.Code != 403 {
		t.Fatalf("member scheduled global check: %d", rec.Code)
	}
}

func TestFailedUpdateCheckPreservesObservation(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	old := publicBuild{BuildID: "123", ObservedAt: time.Now().UTC().Add(-time.Hour)}
	if err := db.KVSet(t.Context(), publicBuildKey, old); err != nil {
		t.Fatal(err)
	}
	fake.CreateErr = errors.New("Docker unavailable")
	j, err := rt.Supervisor().inst.submitUpdateCheck(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, j.ID); got.Status != "failed" {
		t.Fatalf("job=%+v", got)
	}
	var observed publicBuild
	if _, err := db.KVGet(t.Context(), publicBuildKey, &observed); err != nil {
		t.Fatal(err)
	}
	if observed != old {
		t.Fatalf("observation changed: %+v", observed)
	}
}

func TestUpdateCheckRetriesMetadataRead(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	reply := steamReply(t)
	attempt := 0
	fake.OnStart = func(c *runtime.FakeContainer) {
		attempt++
		if attempt == 1 {
			c.Exit(1)
			return
		}
		c.Stdout(reply)
		c.Exit(0)
	}
	j, err := rt.Supervisor().inst.submitUpdateCheck(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, j.ID); got.Status != "succeeded" {
		t.Fatalf("job=%+v", got)
	}
	if fake.Runs() != 2 {
		t.Errorf("query runs=%d, want 2", fake.Runs())
	}
	var found publicBuild
	if ok, err := db.KVGet(t.Context(), publicBuildKey, &found); err != nil || !ok {
		t.Fatalf("cached=%v %v", ok, err)
	}
}

func seedGlobalCheck(t *testing.T, db *store.DB) string {
	t.Helper()
	j := &store.Job{
		ID:      store.NewID(),
		Kind:    jobs.KindUpdateCheck.String(),
		LockKey: jobs.GlobalLockKey(jobs.KindUpdateCheck),
		Payload: "{}",
	}
	if err := db.ClaimJob(t.Context(), j, "dead:boot", time.Now().Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

func TestUpdateCheckSkipsHeldLockAndRecovers(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	held := seedGlobalCheck(t, db)
	scheduleID := seedScheduleRow(t, db, "update_check", nil, time.Now().Add(-time.Minute))
	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now())
	var code string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT error_code FROM job_runs WHERE schedule_id=?`, scheduleID).
		Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "job_in_progress" || fake.Runs() != 0 {
		t.Fatalf("skip=%q runs=%d", code, fake.Runs())
	}
	reply := steamReply(t)
	fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout(reply); c.Exit(0) }
	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	var resumed string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT id FROM job_runs WHERE kind='update_check' AND id<>? AND status<>'cancelled'`, held).
		Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, resumed); got.Status != "succeeded" {
		t.Fatalf("recovered=%+v", got)
	}
	if jobRow(t, db, held).Status != "failed" {
		t.Fatal("dead job not closed")
	}
	if err := rt.Supervisor().Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Runs() != 1 {
		t.Fatalf("recovery reran a completed check: %d", fake.Runs())
	}
	if jobs.ResumeIntentHonoured(jobs.KindUpdateCheck) {
		t.Fatal("update check can restart a game")
	}
}

func TestCancelledUpdateCheckDoesNotPublish(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	started, release := make(chan struct{}), make(chan struct{})
	reply := steamReply(t)
	fake.OnStart = func(c *runtime.FakeContainer) {
		close(started)
		<-release
		c.Stdout(reply)
		c.Exit(0)
	}
	j, err := rt.Supervisor().inst.submitUpdateCheck(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("query did not start")
	}
	err = db.RequestJobCancel(t.Context(), j.ID, time.Now())
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, j.ID); got.Status != "cancelled" {
		t.Fatalf("job=%+v", got)
	}
	var observed publicBuild
	if found, err := db.KVGet(t.Context(), publicBuildKey, &observed); err != nil || found {
		t.Fatalf("published=%v: %v", found, err)
	}
	all, err := fake.List(t.Context(), nil)
	if err != nil || len(all) != 0 {
		t.Fatalf("throwaway leaked: %d %v", len(all), err)
	}
}
