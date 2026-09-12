package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/store"
)

const rotatePath = "/api/v1/admin/keys/rotate"

// rotateKeeper is the key material provisionWorld hands the router, so an envelope written
// here is one the daemon can open.
func rotateKeeper(t *testing.T) *crypto.Keeper {
	t.Helper()
	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sealPassword seeds one instance whose password is a real envelope under generation one.
func sealPassword(t *testing.T, db *store.DB, k *crypto.Keeper, name string) *store.Instance {
	t.Helper()
	inst := seedStoppedInstance(t, db, name)
	resealPassword(t, db, k, inst.ID)
	return inst
}

// resealPassword rewrites one password under k's current generation.
func resealPassword(t *testing.T, db *store.DB, k *crypto.Keeper, id string) {
	t.Helper()
	envelope, err := k.Encrypt(
		crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(id), []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instances SET password = ? WHERE id = ?`, envelope, id)
}

func storedPassword(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var envelope string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT password FROM instances WHERE id = ?`, id).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func activeKeyID(t *testing.T, db *store.DB) string {
	t.Helper()
	var id string
	if _, err := db.KVGet(t.Context(), "active_key_id", &id); err != nil {
		t.Fatal(err)
	}
	return id
}

func rotate(t *testing.T, rt *Router, u *store.User) jobView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodPost, rotatePath, http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/v1/jobs/") {
		t.Errorf("Location = %q, want the job resource", loc)
	}
	var submitted jobView
	decodeInto(t, rec, &submitted)
	return waitJob(t, rt, u, submitted.JobID)
}

// TestRotatingDerivedKeysReSealsEverySecret asserts the contract of 10 §3.3: the write
// generation moves, every encrypted column is rewritten under it, and the plaintext is
// unchanged.
func TestRotatingDerivedKeysReSealsEverySecret(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	k := rotateKeeper(t)
	inst := sealPassword(t, db, k, "rotate-me")

	done := rotate(t, rt, admin)
	if done.Status != "succeeded" {
		t.Fatalf("rotation %s: %+v", done.Status, done)
	}
	if id := activeKeyID(t, db); id != "2" {
		t.Errorf("active key id = %q, want 2", id)
	}

	envelope := storedPassword(t, db, inst.ID)
	if !strings.HasPrefix(envelope, "v1.2.") {
		t.Errorf("password envelope is %q, want the v1.2. generation", envelope)
	}
	plaintext, err := k.Decrypt(
		crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(inst.ID), envelope)
	if err != nil {
		t.Fatalf("decrypt the re-sealed password: %v", err)
	}
	if string(plaintext) != "hunter2" {
		t.Errorf("password = %q, want hunter2", plaintext)
	}
	// Q26: an operator who reaches for this after an incident is told what it does not do.
	if done.Message == nil || !strings.Contains(*done.Message, "master key is unchanged") {
		t.Errorf("completion message = %v, want the master-key caveat", done.Message)
	}
}

// TestARetriedRotationFinishesTheSweptGeneration asserts that a rotation interrupted partway
// is continued rather than restarted: rows left on an older generation are swept under the
// generation already published, and no second one is minted for the same rotation.
func TestARetriedRotationFinishesTheSweptGeneration(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	k := rotateKeeper(t)
	inst := sealPassword(t, db, k, "swept")

	if done := rotate(t, rt, admin); done.Status != "succeeded" {
		t.Fatalf("first rotation %s: %+v", done.Status, done)
	}

	// What an interrupted sweep leaves behind: a row still sealed under generation one.
	resealPassword(t, db, k, inst.ID)

	if done := rotate(t, rt, admin); done.Status != "succeeded" {
		t.Fatalf("retry %s: %+v", done.Status, done)
	}
	if id := activeKeyID(t, db); id != "2" {
		t.Errorf("active key id = %q, want 2: a retry continues the sweep", id)
	}
	if envelope := storedPassword(t, db, inst.ID); !strings.HasPrefix(envelope, "v1.2.") {
		t.Errorf("outstanding envelope is %q, want the v1.2. generation", envelope)
	}
}

// TestKeyRotationIsInvisibleToAMember asserts 09 §3.3's never-grantable rule with ADR-038's
// answer: a caller who cannot rotate cannot see that the endpoint exists.
func TestKeyRotationIsInvisibleToAMember(t *testing.T) {
	rt, _, _, member := provisionWorld(t)
	rec := as(rt, member, httptest.NewRequest(http.MethodPost, rotatePath, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}
