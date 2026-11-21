package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

const (
	// panelIDKey identifies the database itself, and survives restarts.
	panelIDKey = "panel_id"
	// LockFile sits in data.root, and beside the database when that lives elsewhere. A daemon
	// holds an exclusive flock on each for its whole life, and the kernel drops them the moment
	// the process dies, so a restart after a crash is never kept waiting.
	LockFile = ".valmind.lock"
)

// ErrDaemonLocked reports that another daemon holds the install. It is a startup refusal rather
// than a wait: two daemons on one install is an operator error whose failure mode is two
// workers running one restore. A second host cannot share the database either, since SQLite's
// WAL mode keeps its index in one host's shared memory.
var ErrDaemonLocked = errors.New("another valmind holds this install")

// DaemonLock is one daemon's exclusive claim on its data.root and database directory.
type DaemonLock struct {
	files []*os.File
}

// AcquireDaemonLock takes the flock on LockFile in each distinct directory of dirs, data.root
// first, or refuses with ErrDaemonLocked and holds none of them. Directories are told apart by
// identity, not spelling, since flock refuses a second lock on one file even within a process.
func AcquireDaemonLock(ctx context.Context, dirs ...string) (*DaemonLock, error) {
	l := &DaemonLock{}
	var seen []os.FileInfo
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			_ = l.Release()
			return nil, fmt.Errorf("lock %s: %w", dir, err)
		}
		if slices.ContainsFunc(seen, func(s os.FileInfo) bool { return os.SameFile(s, info) }) {
			continue
		}
		seen = append(seen, info)
		f, err := lockDir(dir)
		if err != nil {
			_ = l.Release()
			return nil, err
		}
		l.files = append(l.files, f)
		slog.InfoContext(ctx, "daemon lock acquired", slog.String("lock", f.Name()))
	}
	return l, nil
}

// lockDir takes the flock on LockFile in dir.
func lockDir(dir string) (*os.File, error) {
	// Opened through an os.Root so the lock cannot land outside dir, which is the same
	// discipline every write under worlds/ follows.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	path := filepath.Join(dir, LockFile)
	f, err := root.OpenFile(LockFile, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s is locked: %w", path, ErrDaemonLocked)
	}
	return f, nil
}

// Release drops every lock.
func (l *DaemonLock) Release() error {
	var errs []error
	for _, f := range l.files {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", f.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Owner returns this process's job lease owner, "<panel_id>:<boot_id>". The panel id
// identifies the database and is created on first start; the boot id is fresh per process,
// which is what makes a job row left behind by a dead daemon recognisable (12 §9.1).
func Owner(ctx context.Context, db *DB) (string, error) {
	var panelID string
	found, err := db.KVGet(ctx, panelIDKey, &panelID)
	if err != nil {
		return "", err
	}
	if !found {
		panelID = randomID()
		if err := db.KVSet(ctx, panelIDKey, panelID); err != nil {
			return "", err
		}
	}
	return panelID + ":" + randomID(), nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error()) // unreachable on Linux; nothing sane to fall back to
	}
	return hex.EncodeToString(b)
}

// DaemonRunning reports whether a daemon holds the lock in any of dirs. It takes and at once
// releases each lock AcquireDaemonLock holds for the daemon's life.
func DaemonRunning(dirs ...string) (bool, error) {
	for _, dir := range dirs {
		held, err := lockHeld(dir)
		if err != nil || held {
			return held, err
		}
	}
	return false, nil
}

// lockHeld reports whether another process holds the lock in dir.
func lockHeld(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(LockFile, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", filepath.Join(dir, LockFile), err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
