package setupblob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPutVerifyAndGC(t *testing.T) {
	s := New(t.TempDir())
	first, err := s.PutBytes([]byte("package archive"))
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
	if err := s.GC(map[string]bool{first: true}); err != nil {
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
	if err := s.GC(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan after GC = %v, want os.ErrNotExist", err)
	}
}

func TestPutRejectsIncompletePayload(t *testing.T) {
	s := New(t.TempDir())
	oldLimit := MaxBlobBytes
	MaxBlobBytes = 4
	t.Cleanup(func() { MaxBlobBytes = oldLimit })
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
