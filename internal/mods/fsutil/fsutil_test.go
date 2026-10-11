package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMkdirAllExactCreatesEveryLevelWithExactMode is the reason this function exists over
// os.MkdirAll: every level it creates, not just the leaf, must carry the exact setgid+0775
// bits regardless of the process umask.
func TestMkdirAllExactCreatesEveryLevelWithExactMode(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	if err := MkdirAllExact(target); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), target} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != DirMode {
			t.Errorf("%s: mode = %o, want %o", dir, got, DirMode)
		}
	}
}

func TestMkdirAllExactIsIdempotentOnAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := MkdirAllExact(root); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllExact(root); err != nil {
		t.Fatalf("second call on an already-existing directory: %v", err)
	}
}

// TestMkdirAllExactErrorsWhenPathIsARegularFile is the collision guard: creating a
// directory where a plain file already sits must fail rather than silently succeed or
// (worse) treat the file as if it were the directory.
func TestMkdirAllExactErrorsWhenPathIsARegularFile(t *testing.T) {
	root := t.TempDir()
	inTheWay := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(inTheWay, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MkdirAllExact(inTheWay); err == nil {
		t.Fatal("MkdirAllExact over an existing regular file returned no error")
	}

	// The collision must not have touched the file's content.
	got, err := os.ReadFile(inTheWay)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Errorf("file content = %q, want unchanged %q", got, "x")
	}
}

// TestMkdirAllExactErrorsWhenAnAncestorIsARegularFile is the same collision one level up:
// MkdirAllExact recurses through filepath.Dir, and a file blocking a *parent* segment
// must fail the same way a file blocking the leaf does.
func TestMkdirAllExactErrorsWhenAnAncestorIsARegularFile(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MkdirAllExact(filepath.Join(blocker, "child")); err == nil {
		t.Fatal("MkdirAllExact through a regular-file ancestor returned no error")
	}
}

// TestReadRegularInRefusesAnythingButARegularFile asserts a regular file is read, a missing
// one reports fs.ErrNotExist, and a named pipe or a symlink out of the root is refused, the
// pipe without blocking on it.
func TestReadRegularInRefusesAnythingButARegularFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	for path, data := range map[string]string{filepath.Join(dir, "plain.cfg"): "body", outside: "secret"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.cfg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.cfg")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	tests := []struct {
		name    string
		want    string
		wantErr error
		refused bool // any error will do: os.Root words an escape its own way
	}{
		{name: "plain.cfg", want: "body"},
		{name: "missing.cfg", wantErr: fs.ErrNotExist},
		{name: "pipe.cfg", wantErr: ErrNotRegular},
		{name: "link.cfg", refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{})
			var raw []byte
			var err error
			go func() {
				defer close(done)
				raw, _, err = ReadRegularIn(root, tt.name)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("ReadRegularIn blocked")
			}
			switch {
			case tt.refused:
				if err == nil {
					t.Errorf("read %q through a symlink out of the root", raw)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
			case err != nil || string(raw) != tt.want:
				t.Errorf("ReadRegularIn = %q, %v; want %q", raw, err, tt.want)
			}
		})
	}
}
