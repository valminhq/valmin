package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

const testCopyID = "update-test-copy"

func updateTestPath() string { return "/api/v1/instances/" + seededInstanceID + "/update-test" }

func sendUpdateTest(t *testing.T, rt *Server, u *store.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return as(rt, u, req)
}

// seedTestCopy inserts the update-test copy of the seeded server in the given state.
func seedTestCopy(t *testing.T, db *store.DB, state string) {
	t.Helper()
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, test_of, created_at, updated_at
	) VALUES (?, 'copy', ?, '/srv/valmin/instances/copy', 2461, 'Server', 'World', 'v1.k.n.ct', ?, ?, ?, ?)`,
		testCopyID, state, testCopyID, seededInstanceID, store.Now(), store.Now())
}

func TestStartingAnUpdateTestNeedsSomethingToTest(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")

	rec := sendUpdateTest(t, w.rt, w.admin, http.MethodPost, updateTestPath(), `{}`)

	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"game"`) {
		t.Fatalf("empty update test = %d %s, want 422 naming the field", rec.Code, rec.Body)
	}
	if n := len(jobsOfKind(t, w.db, "update_test")); n != 0 {
		t.Errorf("%d update_test rows were created by a refused request, want 0", n)
	}
}

func TestAnUpdateTestIsRefusedWhenTheHostHasNoMemoryForTheCopy(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	w.rt.instances.ctl.UpdateTester.AvailableMemoryMB = func() (int, error) { return 100, nil }

	rec := sendUpdateTest(t, w.rt, w.admin, http.MethodPost, updateTestPath(), `{"game":true}`)

	if rec.Code != http.StatusConflict || errCode(t, rec) != "insufficient_memory" {
		t.Fatalf("update test with no memory = %d %s, want 409 insufficient_memory", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"available_mb":100`) {
		t.Errorf("the refusal does not say how much memory there is: %s", rec.Body)
	}
	if n := len(jobsOfKind(t, w.db, "update_test")); n != 0 {
		t.Errorf("%d update_test rows were created by a refused request, want 0", n)
	}
}

func TestAServerHasOneUpdateTestCopyAtATime(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	w.rt.instances.ctl.UpdateTester.AvailableMemoryMB = func() (int, error) { return 1 << 20, nil }
	seedTestCopy(t, w.db, "stopped")

	rec := sendUpdateTest(t, w.rt, w.admin, http.MethodPost, updateTestPath(), `{"mods":true}`)

	if rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Fatalf("second update test = %d %s, want 409 invalid_state", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), testCopyID) {
		t.Errorf("the refusal does not name the existing copy: %s", rec.Body)
	}
}

func TestTheReportIsNullWithoutACopyAndNamesTheCopyWithOne(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	type report struct {
		Test *struct {
			CopyID    string `json:"copy_id"`
			CopyState string `json:"copy_state"`
		} `json:"test"`
	}
	read := func() report {
		rec := sendUpdateTest(t, w.rt, w.admin, http.MethodGet, updateTestPath(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("report = %d %s, want 200", rec.Code, rec.Body)
		}
		var got report
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := read(); got.Test != nil {
		t.Fatalf("report without a copy = %+v, want null", got.Test)
	}

	// A copy whose files are missing, as after a failed clone, is still reported so it can be
	// discarded.
	seedTestCopy(t, w.db, "error")
	got := read()
	if got.Test == nil || got.Test.CopyID != testCopyID || got.Test.CopyState != "error" {
		t.Errorf("report with a failed copy = %+v, want the copy in error", got.Test)
	}
}

func TestApplyNeedsASettledCopy(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	apply := func() *httptest.ResponseRecorder {
		return sendUpdateTest(t, w.rt, w.admin, http.MethodPost, updateTestPath()+"/apply", "")
	}

	if rec := apply(); rec.Code != http.StatusNotFound {
		t.Errorf("apply without a copy = %d %s, want 404", rec.Code, rec.Body)
	}

	seedTestCopy(t, w.db, "provisioning")
	if rec := apply(); rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Errorf("apply of a copy still being created = %d %s, want 409 invalid_state", rec.Code, rec.Body)
	}
	// A copy that is stopped but whose test never ran to the end may hold only part of the update.
	seed(t, w.db, `UPDATE instances SET state = 'stopped' WHERE id = ?`, testCopyID)
	if rec := apply(); rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Errorf("apply of an untested copy = %d %s, want 409 invalid_state", rec.Code, rec.Body)
	}
	if n := len(jobsOfKind(t, w.db, "update_apply")); n != 0 {
		t.Errorf("%d update_apply rows were created by refused requests, want 0", n)
	}
}

func TestOnlyAnAdministratorCanTestAnUpdate(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")

	rec := sendUpdateTest(t, w.rt, w.member, http.MethodPost, updateTestPath(), `{"game":true}`)

	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("member's update test = %d %s, want 403 or 404", rec.Code, rec.Body)
	}
	if n := len(jobsOfKind(t, w.db, "update_test")); n != 0 {
		t.Errorf("%d update_test rows were created for a member, want 0", n)
	}
}

type copyReport struct {
	Test *struct {
		CopyID     string `json:"copy_id"`
		Applicable bool   `json:"applicable"`
	} `json:"test"`
}

func readCopyReport(t *testing.T, rt *Server, u *store.User) copyReport {
	t.Helper()
	rec := sendUpdateTest(t, rt, u, http.MethodGet, updateTestPath(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s, want 200", rec.Code, rec.Body)
	}
	var got copyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestOnlyThoseWhoCanSeeTheCopyAreToldAboutIt asserts a member with a view grant on the server,
// which the copy does not inherit, is told the server has no copy.
func TestOnlyThoseWhoCanSeeTheCopyAreToldAboutIt(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	seedTestCopy(t, w.db, "stopped")

	if got := readCopyReport(t, w.rt, w.member); got.Test != nil {
		t.Errorf("a viewer's report = %+v, want null", got.Test)
	}
	if got := readCopyReport(t, w.rt, w.admin); got.Test == nil || got.Test.CopyID != testCopyID {
		t.Errorf("the administrator's report = %+v, want the copy", got.Test)
	}
}

// TestACopyIsApplicableOnceItsTestRanToTheEnd asserts the recorded pass, not the job history,
// decides it, so it stays true however many jobs the server runs afterwards.
func TestACopyIsApplicableOnceItsTestRanToTheEnd(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	seedTestCopy(t, w.db, "stopped")

	if got := readCopyReport(t, w.rt, w.admin); got.Test == nil || got.Test.Applicable {
		t.Fatalf("report of an untested copy = %+v, want it not applicable", got.Test)
	}
	seed(t, w.db, `UPDATE instances SET test_passed_at = ? WHERE id = ?`, store.Now(), testCopyID)
	if got := readCopyReport(t, w.rt, w.admin); got.Test == nil || !got.Test.Applicable {
		t.Errorf("report of a tested copy = %+v, want it applicable", got.Test)
	}
}
