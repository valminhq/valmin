// Package setupblob stores immutable package archives by SHA-256 for saved setups.
package setupblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxBlobBytes bounds a package archive, including a fallback archive made from installed
// files. Extraction separately bounds its uncompressed content.
var MaxBlobBytes = int64(3 << 30)

var (
	ErrInvalidDigest = errors.New("invalid setup blob digest")
	ErrCorruptBlob   = errors.New("setup blob checksum mismatch")
	ErrTooLarge      = errors.New("setup blob exceeds size limit")
)

// Store holds blobs under dataRoot/setups/blobs. Its zero value is not usable.
type Store struct {
	root string
}

func New(dataRoot string) *Store {
	return &Store{root: filepath.Join(dataRoot, "setups", "blobs")}
}

// PutFile retains a regular file as a blob. A caller may remove the original file after
// this returns; the blob has its own inode and has been synced to disk.
func (s *Store) PutFile(path string) (string, error) {
	return s.PutFileContext(context.Background(), path)
}

// PutFileContext retains a regular file while honoring cancellation.
func (s *Store) PutFileContext(ctx context.Context, path string) (string, error) {
	// #nosec G304 -- The caller selects a server-owned package archive.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open setup payload: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat setup payload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("setup payload is not a regular file")
	}
	return s.Put(ctx, f)
}

// PutBytes retains a small in-memory payload, such as a generated fallback archive.
func (s *Store) PutBytes(data []byte) (string, error) {
	return s.Put(context.Background(), bytes.NewReader(data))
}

// Put streams a payload to a temporary file, hashes its bytes, then publishes it under
// its digest. Concurrent identical writes converge on the same final path.
func (s *Store) Put(ctx context.Context, src io.Reader) (string, error) {
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return "", fmt.Errorf("create setup blob directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.root, ".part-")
	if err != nil {
		return "", fmt.Errorf("create setup blob temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(0o640); err != nil {
		return "", fmt.Errorf("chmod setup blob temp file: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(tmp, h), contextReader{ctx: ctx, reader: src}, MaxBlobBytes+1)
	switch {
	case copyErr == nil:
		return "", ErrTooLarge
	case !errors.Is(copyErr, io.EOF):
		return "", fmt.Errorf("copy setup payload: %w", copyErr)
	case n == 0:
		return "", errors.New("setup payload is empty")
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync setup blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close setup blob: %w", err)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(s.root, digest)); err != nil {
		return "", fmt.Errorf("publish setup blob: %w", err)
	}
	if err := syncDir(s.root); err != nil {
		return "", fmt.Errorf("sync setup blob directory: %w", err)
	}
	return digest, nil
}

// Verify hashes a stored blob and returns its path only when it still matches its digest.
func (s *Store) Verify(digest string) (string, error) {
	if !validDigest(digest) {
		return "", ErrInvalidDigest
	}
	path := filepath.Join(s.root, digest)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat setup blob %s: %w", digest, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrCorruptBlob, digest)
	}
	// #nosec G304 -- Digest validation confines this path to the blob directory.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open setup blob %s: %w", digest, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxBlobBytes+1))
	if err != nil {
		return "", fmt.Errorf("hash setup blob %s: %w", digest, err)
	}
	if n > MaxBlobBytes || hex.EncodeToString(h.Sum(nil)) != digest {
		return "", fmt.Errorf("%w: %s", ErrCorruptBlob, digest)
	}
	return path, nil
}

// GC removes unreferenced blobs. Call only while setup save jobs are idle, since a blob
// written by an unfinished save has not yet acquired its database reference.
func (s *Store) GC(referenced map[string]bool) error {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list setup blobs: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validDigest(name) || referenced[name] {
			continue
		}
		if err := os.Remove(filepath.Join(s.root, name)); err != nil {
			return fmt.Errorf("remove orphan setup blob %s: %w", name, err)
		}
	}
	return syncDir(s.root)
}

func validDigest(digest string) bool {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read setup payload: %w", err)
	}
	n, err := r.reader.Read(p)
	if err != nil {
		return n, fmt.Errorf("read setup payload: %w", err)
	}
	return n, nil
}

func syncDir(path string) error {
	// #nosec G304 -- The directory is owned by the blob store.
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open setup blob directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync setup blob directory: %w", err)
	}
	return nil
}
