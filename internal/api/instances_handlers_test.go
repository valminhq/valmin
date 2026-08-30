package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/crypto"
)

// TestListInstancesIsGrantScoped is 09 §1: admin sees everything, a member sees only what
// they hold a live grant on.
func TestListInstancesIsGrantScoped(t *testing.T) {
	rt, _, admin, member := world(t)

	adminList := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody))
	var adminPage struct {
		Items []struct{ ID string } `json:"items"`
	}
	decodeInto(t, adminList, &adminPage)
	if len(adminPage.Items) != 2 {
		t.Errorf("admin sees %d instances, want 2", len(adminPage.Items))
	}

	memberList := as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody))
	var memberPage struct {
		Items []struct{ ID string } `json:"items"`
	}
	decodeInto(t, memberList, &memberPage)
	if len(memberPage.Items) != 1 || memberPage.Items[0].ID != "inst-a" {
		t.Errorf("member sees %+v, want just inst-a", memberPage.Items)
	}
}

// TestGetInstanceInvisibleIsNotFound is D2 / ADR-038, exercised at this endpoint too.
func TestGetInstanceInvisibleIsNotFound(t *testing.T) {
	rt, _, _, member := world(t)
	rec := as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-b", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestGetInstanceNeverContainsPassword is 05 M1's own acceptance test, checked at the
// wire level rather than trusting the struct shape.
func TestGetInstanceNeverContainsPassword(t *testing.T) {
	rt, _, admin, _ := world(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("password")) {
		t.Errorf("response body mentions password: %s", rec.Body)
	}
}

// TestPatchInstanceLimitsRequiresTheAction proves InstanceLimits' never-grantable status
// (09 §3.3) holds even for a member who can otherwise see the instance.
func TestPatchInstanceLimitsRequiresTheAction(t *testing.T) {
	rt, _, admin, member := world(t)

	mem := 8192
	body := jsonBody(t, map[string]int{"mem_limit_mb": mem})
	memberRec := as(rt, member, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a", body))
	if memberRec.Code != http.StatusForbidden {
		t.Errorf("member patch limits = %d, want 403 (%s)", memberRec.Code, memberRec.Body)
	}

	adminRec := as(rt, admin, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a", jsonBody(t, map[string]int{"mem_limit_mb": mem}),
	))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin patch limits = %d, want 200 (%s)", adminRec.Code, adminRec.Body)
	}
	var updated struct {
		MemLimitMB      int  `json:"mem_limit_mb"`
		RestartRequired bool `json:"restart_required"`
	}
	decodeInto(t, adminRec, &updated)
	if updated.MemLimitMB != mem {
		t.Errorf("mem_limit_mb = %d, want %d", updated.MemLimitMB, mem)
	}
	if !updated.RestartRequired {
		t.Error("restart_required not set (12 §2.5)")
	}
}

// TestPasswordEndpointDecryptsAndAudits proves the endpoint returns the plaintext game
// secret, not the stored envelope, and writes the audit_log row 11 §9 requires.
func TestPasswordEndpointDecryptsAndAudits(t *testing.T) {
	rt, db, admin, member := world(t)

	// The same keeper world()'s router was built with (router_test.go's routerWithDB).
	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := k.Encrypt(
		crypto.PurposeInstancePassword, crypto.Location{Table: "instances", Column: "password", RowID: "inst-a"},
		[]byte("s3cret!"),
	)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instances SET password = ? WHERE id = 'inst-a'`, envelope)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/password", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Password string `json:"password"`
	}
	decodeInto(t, rec, &got)
	if got.Password != "s3cret!" {
		t.Errorf("password = %q, want the decrypted plaintext", got.Password)
	}

	var auditCount int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE instance_id = 'inst-a' AND action = 'instances.password.read'`,
	).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Errorf("audit_log rows = %d, want 1", auditCount)
	}

	// A member without instance.view on inst-b never reaches the decrypt path.
	rec = as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-b/password", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("member password read on B = %d, want 404", rec.Code)
	}
}

// TestAcknowledgeRequiresErrorState covers 12 §2.4's guard: acknowledge is the only exit
// from `error`, and nothing else.
func TestAcknowledgeRequiresErrorState(t *testing.T) {
	rt, _, admin, _ := world(t)
	// inst-a starts 'stopped' in world().
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/acknowledge", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("acknowledge from stopped = %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

// TestAcknowledgeReconcilesToStopped covers the common case: an instance parked in `error`
// with no container reconciles to `stopped` (12 §2.4).
func TestAcknowledgeReconcilesToStopped(t *testing.T) {
	rt, db, admin, member := world(t)
	seed(t, db, `UPDATE instances SET state = 'error' WHERE id = 'inst-a'`)

	// A member with only a viewer grant can still see and acknowledge — 09 §3.1 gives
	// viewer no instance.view-gated action beyond view itself, and acknowledge is gated on
	// instance.view here, matching the same visibility rule as get/password.
	forbidden := as(
		rt,
		member,
		httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-b/acknowledge", http.NoBody),
	)
	if forbidden.Code != http.StatusNotFound {
		t.Errorf("member acknowledge B = %d, want 404", forbidden.Code)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/acknowledge", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var updated struct {
		State string `json:"state"`
	}
	decodeInto(t, rec, &updated)
	if updated.State != "stopped" {
		t.Errorf("state = %q, want stopped (no container to reconcile against)", updated.State)
	}
}
