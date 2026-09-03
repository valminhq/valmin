// Package backup owns the archive primitive over a stopped instance's worlds tree.
//
// Specification: 02 §4.4, 03 §4.1.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Result describes one finished archive.
type Result struct {
	Path      string
	SHA256    string
	SizeBytes int64
	// Entries is the count of regular files written, so a caller can refuse an archive that
	// captured nothing (02 §4.4 step 5's "verify the archive lists a .db and .fwl").
	Entries int
}

// partSuffix marks an archive that is still being written. 12 §9.4: a `.part` file found on
// recovery is deleted unconditionally, and no catalogue row exists until the rename
// succeeds — which is what makes a partial archive structurally invisible rather than
// something a reader has to be careful about.
const partSuffix = ".part"

// Archive writes worldsDir as a gzipped tar at dest, atomically.
//
// gzip rather than zstd: 02 §4.4 permits either, and gzip is in the standard library while
// zstd would be a dependency bought for an unmeasured compression ratio.
//
// The archive is written to `<dest>.part` and renamed only after the gzip and tar streams
// have both been closed, because closing is what flushes them — a rename before it
// publishes a truncated archive that still looks complete. The hash is computed over the
// bytes as they are written rather than by re-reading the file, so it describes what
// actually landed.
//
// The caller is responsible for the instance being stopped. This function performs no
// quiesce: its only caller is world import, which already requires `stopped`. A caller
// that needs a hot copy has to wrap it in the stop-and-wait sequence itself.
func Archive(worldsDir, dest string) (Result, error) {
	part := dest + partSuffix
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return Result{}, fmt.Errorf("create archive directory: %w", err)
	}
	// dest is panel-built from data.root and an instance id; no request value reaches it.
	f, err := os.Create(part) //nolint:gosec // see above
	if err != nil {
		return Result{}, fmt.Errorf("create archive: %w", err)
	}
	published := false
	defer func() {
		_ = f.Close()
		if !published {
			_ = os.Remove(part)
		}
	}()

	hash := sha256.New()
	counter := &countingWriter{}
	gz := gzip.NewWriter(io.MultiWriter(f, hash, counter))
	tw := tar.NewWriter(gz)

	entries, err := writeTree(tw, worldsDir)
	if err != nil {
		return Result{}, err
	}
	// Both closes flush; neither is optional, and a deferred close would run too late to
	// be part of the hash.
	if err := tw.Close(); err != nil {
		return Result{}, fmt.Errorf("finish tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Result{}, fmt.Errorf("finish gzip: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Result{}, fmt.Errorf("flush archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(part, dest); err != nil {
		return Result{}, fmt.Errorf("publish archive: %w", err)
	}
	published = true

	return Result{
		Path: dest, SHA256: hex.EncodeToString(hash.Sum(nil)),
		SizeBytes: counter.n, Entries: entries,
	}, nil
}

// writeTree walks root and writes every regular file into tw, with paths relative to root.
//
// Only regular files and directories. A symlink inside worlds/ would otherwise be
// archived as a link that a later restore could follow out of the tree, which is the same
// class of hole as an archive entry named `../` (B5).
func writeTree(tw *tar.Writer, root string) (int, error) {
	entries := 0
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path of %s: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}

		if err := writeEntry(tw, path, rel, fi); err != nil {
			return err
		}
		if !fi.IsDir() {
			entries++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("archive %s: %w", root, err)
	}
	return entries, nil
}

// Name builds an archive filename carrying the instance and the moment, never a raw path
// (11 §8.3).
func Name(instanceName, stamp string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, instanceName)
	return safe + "-" + stamp + ".tar.gz"
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// writeEntry writes one header, and the file body when there is one.
func writeEntry(tw *tar.Writer, path, rel string, fi os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return fmt.Errorf("header for %s: %w", rel, err)
	}
	hdr.Name = filepath.ToSlash(rel)
	if fi.IsDir() {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header for %s: %w", rel, err)
	}
	if fi.IsDir() {
		return nil
	}

	// path comes from Walk over the caller's own worlds dir, never from a request.
	src, err := os.Open(path) //nolint:gosec // see above
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = src.Close() }()
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("archive %s: %w", rel, err)
	}
	return nil
}
