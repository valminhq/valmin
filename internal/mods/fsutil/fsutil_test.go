package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
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
