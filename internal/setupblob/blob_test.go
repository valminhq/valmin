package setupblob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPutVerifyAndGC(t *testing.T) {
	s := New(t.TempDir())
	first, err := s.PutBytes(t.Context(), []byte("package archive"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "other.zip")
	if err := os.WriteFile(src, []byte("package archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := s.PutFile(src)
	if err != nil || first != second {
		t.Fatalf("duplicate PutFile = %q, %v, want %q", second, err, first)
	}
	path, err := s.Verify(first)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := s.GC(map[string]bool{first: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("GC removed a referenced blob: %v", err)
	}
	if _, err := s.Verify("../" + first); !errors.Is(err, ErrInvalidDigest) {
		t.Errorf("unsafe digest = %v, want ErrInvalidDigest", err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(first); !errors.Is(err, ErrCorruptBlob) {
		t.Errorf("corrupt blob = %v, want ErrCorruptBlob", err)
	}
	if _, err := s.GC(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan after GC = %v, want os.ErrNotExist", err)
	}
}

func TestPutRejectsIncompletePayload(t *testing.T) {
	s := New(t.TempDir())
	s.maxBytes = 4
	if _, err := s.Put(t.Context(), bytes.NewReader([]byte("too large"))); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized Put = %v, want ErrTooLarge", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Put(ctx, bytes.NewReader([]byte("content"))); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Put = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed Put left files: %v", entries)
	}
}

// TestPutBytesReplacesADamagedCopy asserts PutBytes rewrites a stored blob whose bytes no
// longer match its digest, and stores an empty payload.
func TestPutBytesReplacesADamagedCopy(t *testing.T) {
	s := New(t.TempDir())
	digest, err := s.PutBytes(t.Context(), []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, digest), []byte("CONFIG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBytes(t.Context(), []byte("config")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(digest); err != nil {
		t.Errorf("Verify after a second PutBytes = %v, want the blob repaired", err)
	}
	empty, err := s.PutBytes(t.Context(), nil)
	if err != nil {
		t.Fatalf("PutBytes(empty) = %v", err)
	}
	if _, err := s.Verify(empty); err != nil {
		t.Errorf("Verify(empty) = %v", err)
	}
}

// TestPutLinkSharesTheFile asserts PutLink stores a file under its digest as a hard link to it,
// and returns the same digest without a second link for bytes already held.
func TestPutLinkSharesTheFile(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	src := filepath.Join(dir, "cache.zip")
	if err := os.WriteFile(src, []byte("package archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := s.PutLink(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Verify(digest)
	if err != nil {
		t.Fatal(err)
	}
	a, errA := os.Stat(src)
	b, errB := os.Stat(path)
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		t.Errorf("blob is not a link to the source file: %v %v", errA, errB)
	}
	again, err := s.PutLink(t.Context(), src)
	if err != nil || again != digest {
		t.Errorf("second PutLink = %q, %v, want %q", again, err, digest)
	}
}

// TestPutLinkConcurrent asserts that calls retaining the same file at once all succeed with one
// intact blob, as jobs on different servers retaining one package do.
func TestPutLinkConcurrent(t *testing.T) {
	for range 20 {
		dir := t.TempDir()
		s := New(dir)
		src := filepath.Join(dir, "cache.zip")
		if err := os.WriteFile(src, []byte("shared package archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		const callers = 8
		digests := make([]string, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() { digests[i], errs[i] = s.PutLink(t.Context(), src) })
		}
		wg.Wait()
		for i := range callers {
			if errs[i] != nil || digests[i] != digests[0] {
				t.Fatalf("PutLink %d = %q, %v, want %q", i, digests[i], errs[i], digests[0])
			}
		}
		if _, err := s.Verify(digests[0]); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(Dir(dir))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("blob directory holds %d entries, want only the blob", len(entries))
		}
	}
}

// TestGCRemovesInterruptedWrites asserts GC removes the temporary files an interrupted write
// left, and keeps a referenced blob.
func TestGCRemovesInterruptedWrites(t *testing.T) {
	s := New(t.TempDir())
	digest, err := s.PutBytes(t.Context(), []byte("kept"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".part-123", ".link-" + digest} {
		if err := os.WriteFile(filepath.Join(s.root, name), []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GC(map[string]bool{digest: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil || len(entries) != 1 || entries[0].Name() != digest {
		t.Errorf("blobs after GC = %v, %v, want only %s", entries, err, digest)
	}
}

// TestGCSparesAJustLinkedBlob covers a blob hard-linked from an old file: its mtime is the
// file's, but the link moved its ctime, so a GC cutoff an hour back leaves it alone.
func TestGCSparesAJustLinkedBlob(t *testing.T) {
	s := New(t.TempDir())
	src := filepath.Join(t.TempDir(), "Mod-1.0.0.zip")
	if err := os.WriteFile(src, []byte("package archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutLink(t.Context(), src); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC(nil, time.Now().Add(-time.Hour))
	if err != nil || len(removed) != 0 {
		t.Fatalf("GC an hour back = %v, %v, want nothing removed", removed, err)
	}
	removed, err = s.GC(nil, time.Now().Add(time.Minute))
	if err != nil || len(removed) != 1 {
		t.Fatalf("GC past the link = %v, %v, want the orphan removed", removed, err)
	}
	if freed := removed[0].FreedBytes(); freed != 0 {
		t.Errorf("orphan still linked by its source freed %d bytes, want 0", freed)
	}
}
