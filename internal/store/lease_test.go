package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerReusesThePanelIDAndVariesTheBootID(t *testing.T) {
	t.Parallel()
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
		t.Error("boot id repeated; a dead daemon's job rows would look like this boot's")
	}
}

func TestSecondDaemonOnOneDataRootIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	first, err := AcquireDaemonLock(t.Context(), root)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	_, err = AcquireDaemonLock(t.Context(), root)
	if !errors.Is(err, ErrDaemonLocked) {
		t.Fatalf("second acquire on the same data.root: got %v, want ErrDaemonLocked", err)
	}
	if !strings.Contains(err.Error(), LockFile) {
		t.Errorf("error does not name the lock file: %v", err)
	}
}

// TestTwoDataRootsOnOneDatabaseAreRefused asserts the lock beside a database kept outside
// data.root stops a second daemon that names the same database from another data.root, and
// that a refused acquire holds nothing.
func TestTwoDataRootsOnOneDatabaseAreRefused(t *testing.T) {
	t.Parallel()
	shared := t.TempDir()
	firstRoot, secondRoot := t.TempDir(), t.TempDir()

	first, err := AcquireDaemonLock(t.Context(), firstRoot, shared)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireDaemonLock(t.Context(), secondRoot, shared); !errors.Is(err, ErrDaemonLocked) {
		t.Fatalf("second acquire on the shared database: got %v, want ErrDaemonLocked", err)
	}
	if running, err := DaemonRunning(secondRoot); err != nil || running {
		t.Errorf("the refused acquire still holds its data.root: DaemonRunning = %v, %v", running, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestOneDirectoryUnderTwoNamesIsLockedOnce asserts a daemon whose database directory is its
// data.root under another spelling does not refuse itself.
func TestOneDirectoryUnderTwoNamesIsLockedOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}

	l, err := AcquireDaemonLock(t.Context(), root, alias)
	if err != nil {
		t.Fatalf("acquire through two names for one directory: %v", err)
	}
	if running, err := DaemonRunning(alias); err != nil || !running {
		t.Errorf("DaemonRunning through the alias = %v, %v; want true", running, err)
	}
	_ = l.Release()
}

func TestReleaseLetsTheNextStartInImmediately(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	first, err := AcquireDaemonLock(t.Context(), root)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := AcquireDaemonLock(t.Context(), root)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = second.Release()
}

func TestLockFileLivesUnderDataRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	l, err := AcquireDaemonLock(t.Context(), root)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })

	if _, err := os.Stat(filepath.Join(root, LockFile)); err != nil {
		t.Errorf("stat lock file: %v", err)
	}
}

// TestDaemonRunningFollowsTheLock asserts that DaemonRunning reports the lock a daemon holds,
// and leaves it free for a daemon to take when there is none.
func TestDaemonRunningFollowsTheLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if running, err := DaemonRunning(root); err != nil || running {
		t.Fatalf("DaemonRunning with no daemon = %v, %v; want false", running, err)
	}
	lock, err := AcquireDaemonLock(t.Context(), root)
	if err != nil {
		t.Fatalf("acquire after the probe: %v", err)
	}
	if running, err := DaemonRunning(root); err != nil || !running {
		t.Errorf("DaemonRunning with a daemon = %v, %v; want true", running, err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if running, err := DaemonRunning(root); err != nil || running {
		t.Errorf("DaemonRunning after release = %v, %v; want false", running, err)
	}
}
