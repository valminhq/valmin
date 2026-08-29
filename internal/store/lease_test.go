package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testTTL = 30 * time.Second

func TestOwnerReusesThePanelIDAndVariesTheBootID(t *testing.T) {
	db := open(t)

	first, err := Owner(t.Context(), db)
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	second, err := Owner(t.Context(), db)
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}

	firstPanel, firstBoot, _ := strings.Cut(first, ":")
	secondPanel, secondBoot, _ := strings.Cut(second, ":")
	if firstPanel != secondPanel {
		t.Errorf("panel id changed across calls: %q then %q", firstPanel, secondPanel)
	}
	if firstBoot == secondBoot {
		t.Error("boot id repeated; a dead daemon's job rows would look like this boot's (12 §9.1)")
	}
}

func TestSecondDaemonOnOneHostIsRefusedByTheFlock(t *testing.T) {
	db := open(t)
	root := t.TempDir()

	first, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release(t.Context()) })

	_, err = AcquireDaemonLease(t.Context(), db, root, "panel:boot-2", testTTL)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire on the same data.root: got %v, want ErrLeaseHeld", err)
	}
	if !strings.Contains(err.Error(), LockFile) {
		t.Errorf("error does not name the lock file: %v", err)
	}
}

// The flock only reaches one host. Two hosts pointed at one database meet at the kv row,
// which is the case ADR-031 exists for.
func TestSecondDaemonOnAnotherHostIsRefusedByTheLeaseRow(t *testing.T) {
	db := open(t)

	first, err := AcquireDaemonLease(t.Context(), db, t.TempDir(), "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release(t.Context()) })

	_, err = AcquireDaemonLease(t.Context(), db, t.TempDir(), "panel:boot-2", testTTL)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire against the same database: got %v, want ErrLeaseHeld", err)
	}

	host, _ := os.Hostname()
	for _, want := range []string{host, "pid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q, so the operator cannot find the other daemon: %v", want, err)
		}
	}
}

// After a SIGKILL the lock file is closed by the kernel but the lease row survives, so the
// restart waits out the TTL and no longer. 30 s of self-healing delay is ADR-031's stated
// cost; a restart blocked until a human intervenes would not be.
func TestAHardKillBlocksARestartForNoLongerThanTheTTL(t *testing.T) {
	db := open(t)
	root := t.TempDir()

	killed, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A SIGKILL releases the flock and nothing else: no Release, no row deleted.
	if err := killed.lock.Close(); err != nil {
		t.Fatalf("close lock: %v", err)
	}

	if _, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-2", testTTL); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("restart inside the TTL: got %v, want ErrLeaseHeld", err)
	}

	expire(t, db, -time.Second)

	next, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-2", testTTL)
	if err != nil {
		t.Fatalf("restart after the TTL expired: %v", err)
	}
	_ = next.Release(t.Context())
}

func TestReleaseLetsTheNextStartInImmediately(t *testing.T) {
	db := open(t)
	root := t.TempDir()

	first, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-2", testTTL)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = second.Release(t.Context())
}

// C17's shape at the daemon level: a renewal that finds another owner must stop this
// daemon, because continuing is the two-workers-one-restore failure ADR-031 removes.
func TestRenewalRefusesAfterTheLeaseIsStolen(t *testing.T) {
	db := open(t)

	l, err := AcquireDaemonLease(t.Context(), db, t.TempDir(), "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = l.Release(t.Context()) })

	if err := db.KVSet(t.Context(), leaseKey, Lease{
		Owner:     "panel:boot-2",
		Host:      "elsewhere",
		PID:       4242,
		ExpiresAt: time.Now().UTC().Add(testTTL),
	}); err != nil {
		t.Fatalf("steal the lease: %v", err)
	}

	if err := l.claim(t.Context()); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("renewal after theft: got %v, want ErrLeaseHeld", err)
	}
}

func TestReleaseLeavesAStolenLeaseAlone(t *testing.T) {
	db := open(t)

	l, err := AcquireDaemonLease(t.Context(), db, t.TempDir(), "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	stolen := Lease{
		Owner: "panel:boot-2", Host: "elsewhere", PID: 4242,
		ExpiresAt: time.Now().UTC().Add(testTTL),
	}
	if err := db.KVSet(t.Context(), leaseKey, stolen); err != nil {
		t.Fatalf("steal the lease: %v", err)
	}

	if err := l.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}

	var got Lease
	found, err := db.KVGet(t.Context(), leaseKey, &got)
	if err != nil {
		t.Fatalf("KVGet: %v", err)
	}
	if !found || got.Owner != stolen.Owner {
		t.Errorf("release deleted the live owner's lease: found=%v owner=%q", found, got.Owner)
	}
}

func TestLockFileLivesUnderDataRoot(t *testing.T) {
	db := open(t)
	root := t.TempDir()

	l, err := AcquireDaemonLease(t.Context(), db, root, "panel:boot-1", testTTL)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = l.Release(t.Context()) })

	if _, err := os.Stat(filepath.Join(root, LockFile)); err != nil {
		t.Errorf("stat lock file: %v", err)
	}
}

// expire shifts the stored lease's expiry by d, so a test does not have to wait one out.
func expire(t *testing.T, db *DB, d time.Duration) {
	t.Helper()
	var l Lease
	found, err := db.KVGet(t.Context(), leaseKey, &l)
	if err != nil || !found {
		t.Fatalf("read lease: found=%v err=%v", found, err)
	}
	l.ExpiresAt = time.Now().UTC().Add(d)
	if err := db.KVSet(t.Context(), leaseKey, l); err != nil {
		t.Fatalf("write lease: %v", err)
	}
}
