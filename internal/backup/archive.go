// Package backup owns the archive primitive over a stopped instance's worlds tree, and the
// atomic directory swap that publishes one: a restore replaces worlds_local with it, and a
// game update replaces server/ with it (ADR-139). Two callers, one answer to being killed
// between the renames.
//
// Specification: 02 §4.4, 03 §4.1, 12 §9.4.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

// PartSuffix marks an archive that is still being written. A `.part` file found on recovery is
// deleted unconditionally, and no catalogue row exists until the rename succeeds (12 §9.4).
const PartSuffix = ".part"

// Archive writes worldsDir as a gzipped tar at dest, atomically. gzip rather than zstd: 02 §4.4
// permits either, and gzip is in the standard library.
//
// Written to `<dest>.part` and renamed only after both streams are closed, since closing is what
// flushes them; a rename before it would publish a truncated archive that looks complete. The
// hash is computed over the bytes as they are written, describing what actually landed.
//
// The caller is responsible for the instance being stopped: this function performs no quiesce,
// and any file that changes during the copy fails it. ArchiveLive is the copy of a running world.
func Archive(worldsDir, dest string) (Result, error) { return archive(worldsDir, dest, false) }

// ArchiveLive archives a world the game may be writing. A file that vanishes during the copy is
// left out, and one whose size changes is archived at the size the walk saw, cut or zero-padded,
// so the archive is readable but not consistent.
func ArchiveLive(worldsDir, dest string) (Result, error) { return archive(worldsDir, dest, true) }

func archive(worldsDir, dest string, live bool) (Result, error) {
	part := dest + PartSuffix
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

	entries, err := writeTree(tw, worldsDir, live)
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

// writeTree walks root and writes every regular file into tw, with paths relative to root. Only
// regular files and directories: a symlink would archive as a link a later restore could follow
// out of the tree (B5). Every read goes through an os.Root, so a path the game swaps for a
// symlink while the copy runs can't make the panel read a file outside the tree.
func writeTree(tw *tar.Writer, root string, live bool) (int, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return 0, fmt.Errorf("archive %s: %w", root, err)
	}
	defer func() { _ = dir.Close() }()
	w := &treeWriter{tw: tw, dir: dir, live: live}
	if err := fs.WalkDir(dir.FS(), ".", w.visit); err != nil {
		return 0, fmt.Errorf("archive %s: %w", root, err)
	}
	return w.entries, nil
}

// treeWriter is one writeTree walk.
type treeWriter struct {
	tw      *tar.Writer
	dir     *os.Root
	live    bool
	entries int
}

func (w *treeWriter) visit(rel string, d fs.DirEntry, err error) error {
	if w.live && errors.Is(err, fs.ErrNotExist) && rel != "." {
		return nil
	}
	if err != nil || rel == "." {
		return err
	}
	if !d.Type().IsRegular() && !d.IsDir() {
		return nil
	}
	if !archivable(rel) {
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	fi, err := d.Info()
	if w.live && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	files, err := writeEntry(w.tw, w.dir, rel, fi, w.live)
	w.entries += files
	return err
}

// cacheDir holds what the game derives from a world rather than the world itself: on the
// operator's host `<world>_biomedatacache.bin` was 21 MB against a 794 KB world, so an archive
// that carried it was 91% derived data.
const cacheDir = "cache"

// archivable decides what belongs in a world archive, over a slash-separated relative path.
//
// Everything under worlds/ is kept except what the game rebuilds or keeps for itself: the
// derived cache, and the rolling `_backup_auto-*` saves beside the world in worlds_local/. A
// restore carries the rolling saves already on disk across (see SwapKeeping).
func archivable(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if parts[0] == cacheDir {
		return false
	}
	return len(parts) < 2 || parts[0] != worldsLocalDir || !IsAutoSave(parts[1])
}

// worldsLocalDir is the directory under worlds/ the game keeps its saves in.
const worldsLocalDir = "worlds_local"

// autoSave matches the name of one of the game's rolling saves once its extensions are
// removed: `<world>_backup_auto-20260903073824` before 1.0, `<world>_backup_auto-20260913-204651`
// as a directory since.
var autoSave = regexp.MustCompile(`_backup_auto-\d[\d-]*$`)

// IsAutoSave reports whether a file or directory name under worlds_local/ is one of the game's
// rolling saves, in either layout.
func IsAutoSave(name string) bool {
	name = strings.TrimSuffix(name, ".old")
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".db"), ".fwl")
	return autoSave.MatchString(name)
}

// Name builds an archive filename carrying the instance, the moment, and the id of the
// catalogue row that will name it, never a raw path (11 §8.3).
//
// The id is what makes it unique: the stamp has one-second resolution, so two archives of one
// instance taken in the same second would otherwise share a path, and the catalogue would hold
// two rows pointing at one file.
func Name(instanceName, stamp, id string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, instanceName)
	return safe + "-" + stamp + "-" + shortID(id) + ".tar.gz"
}

// shortID is the tail of a uuidv7, which is its random half (06 §4).
func shortID(id string) string {
	compact := strings.ReplaceAll(id, "-", "")
	if len(compact) <= idSuffixLen {
		return compact
	}
	return compact[len(compact)-idSuffixLen:]
}

const idSuffixLen = 8

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// writeEntry writes one header, and the file body when there is one. It returns the number of
// files written: 0 for a directory, or for a file that is no longer a regular file when opened.
// A live copy also skips a file that vanished; a cold one fails on it.
func writeEntry(tw *tar.Writer, dir *os.Root, rel string, fi os.FileInfo, live bool) (int, error) {
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return 0, fmt.Errorf("header for %s: %w", rel, err)
	}
	hdr.Name = rel
	if fi.IsDir() {
		hdr.Name += "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, fmt.Errorf("write header for %s: %w", rel, err)
		}
		return 0, nil
	}

	// O_NONBLOCK so a FIFO planted in place of the file can't hold the open forever.
	src, err := dir.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if live && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP)) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = src.Close() }()
	if opened, err := src.Stat(); err != nil || !opened.Mode().IsRegular() {
		return 0, err //nolint:wrapcheck // nil, or the handle's own stat error
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, fmt.Errorf("write header for %s: %w", rel, err)
	}
	if !live {
		if _, err := io.Copy(tw, src); err != nil {
			return 0, fmt.Errorf("archive %s: %w", rel, err)
		}
		return 1, nil
	}
	n, err := io.CopyN(tw, src, hdr.Size)
	if errors.Is(err, io.EOF) {
		_, err = io.CopyN(tw, zeros{}, hdr.Size-n)
	}
	if err != nil {
		return 0, fmt.Errorf("archive %s: %w", rel, err)
	}
	return 1, nil
}

// zeros pads a file that shrank during a live copy up to the size its header promised.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
