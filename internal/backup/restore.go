package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// Suffixes of the two directories the restore swap uses (12 §9.4). Recovery reads which of the
// three exist, so both names live here rather than at each call site.
const (
	StagedSuffix     = ".new"
	SupersededSuffix = ".old"
)

// ErrUnsafeEntry is an archive entry that resolves outside the destination, or that is neither
// a regular file nor a directory (B5).
var ErrUnsafeEntry = errors.New("archive entry is not safe to extract")

// Extract writes the entries of archivePath that sit under prefix into destDir, with the prefix
// stripped. destDir is created and must be disposable: a failure leaves it partly written, and
// the caller discards it.
//
// No entry is trusted. Its path is re-checked against destDir and its type must be a regular
// file or a directory, the same discipline a mod zip gets (B5, 03 §6.5); modes are this
// package's, never the archive's.
func Extract(archivePath, prefix, destDir string) error {
	// archivePath comes from a catalogue row, never from a request (D13).
	f, err := os.Open(archivePath) //nolint:gosec // see above
	if err != nil {
		return fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
	}
	defer func() { _ = gz.Close() }()

	if err := fsutil.MkdirAllExact(destDir); err != nil {
		return fmt.Errorf("create %s: %w", destDir, err)
	}
	root := filepath.Clean(destDir)

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
		}
		if err := extractEntry(tr, hdr, root, prefix); err != nil {
			return err
		}
	}
}

// extractEntry writes one member, or skips it if it does not sit under prefix. A name that is
// unsafe is refused whether or not it is one this extraction wanted.
func extractEntry(tr *tar.Reader, hdr *tar.Header, root, prefix string) error {
	name, err := cleanEntry(hdr.Name)
	if err != nil {
		return err
	}
	rel, ok := underPrefix(name, prefix)
	if !ok {
		return nil
	}
	dest, err := safeJoin(root, rel)
	if err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := fsutil.MkdirAllExact(dest); err != nil {
			return fmt.Errorf("create %s: %w", rel, err)
		}
		return nil
	case tar.TypeReg:
		return writeExtracted(tr, dest, hdr.Size)
	default:
		return fmt.Errorf("%w: %q is neither a file nor a directory", ErrUnsafeEntry, hdr.Name)
	}
}

// cleanEntry normalises an entry name and refuses one that is absolute or that climbs out of
// the archive's own root. It runs before the prefix filter, so a hostile name is refused rather
// than quietly skipped as belonging to some other subtree.
//
// Normalisation runs first: a name stored with backslashes would otherwise read as one
// harmless filename.
func cleanEntry(name string) (string, error) {
	clean := strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, `\`, "/")), "./")
	if clean == "" || clean == "." || strings.HasPrefix(clean, "/") ||
		clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q is not a path inside the archive", ErrUnsafeEntry, name)
	}
	return clean, nil
}

// underPrefix reports a cleaned entry name relative to prefix, and whether it sits under it at
// all. The prefix directory's own entry is not under itself: destDir already exists.
func underPrefix(name, prefix string) (string, bool) {
	if prefix == "" {
		return name, true
	}
	rel, ok := strings.CutPrefix(name, prefix+"/")
	return rel, ok && rel != ""
}

// safeJoin resolves rel under root and refuses anything that lands outside it. filepath.Join
// cleans as it joins, so the prefix comparison runs after a "../" has already been resolved
// away; scanning for ".." literally misses "a/../../b".
func safeJoin(root, rel string) (string, error) {
	dest := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if dest != root && !strings.HasPrefix(dest, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the destination", ErrUnsafeEntry, rel)
	}
	return dest, nil
}

// writeExtracted writes one entry, copying exactly the size its header declared: fewer bytes is
// a truncated stream, and the bound is what stops a hostile archive from writing more than it
// admitted to.
func writeExtracted(src io.Reader, dest string, size int64) error {
	if err := fsutil.MkdirAllExact(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	// dest is root-checked against the staging directory by safeJoin.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fsutil.FileMode) //nolint:gosec // see above
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer func() { _ = f.Close() }()

	if n, err := io.CopyN(f, src, size); err != nil || n != size {
		return fmt.Errorf("%w: %s is %d of %d bytes: %w", ErrArchiveUnreadable, dest, n, size, err)
	}
	if err := f.Chmod(fsutil.FileMode); err != nil {
		return fmt.Errorf("set mode on %s: %w", dest, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	return nil
}

// Swap publishes the staged world: live is set aside as `.old`, `.new` becomes live, and `.old`
// is removed (12 §9.4). Two renames rather than a copy, so at no point is there a partly
// written world under the name the server loads.
//
// A missing live directory is not an error: a restore into an instance that never had a world
// has nothing to set aside.
func Swap(live string) error {
	if _, err := os.Stat(live); err == nil {
		if err := os.Rename(live, live+SupersededSuffix); err != nil {
			return fmt.Errorf("set the current world aside: %w", err)
		}
	}
	if err := os.Rename(live+StagedSuffix, live); err != nil {
		return fmt.Errorf("publish the restored world: %w", err)
	}
	if err := os.RemoveAll(live + SupersededSuffix); err != nil {
		return fmt.Errorf("remove the superseded world: %w", err)
	}
	return nil
}

// RecoverSwap resolves whatever state an interrupted restore left the three directories in,
// and reports what it did (12 §9.4). Every case leaves exactly one world under live.
//
// It completes the swap wherever the staged world is the only candidate, and reverses it
// wherever the live world is still there: past the first rename the operator's choice has been
// staged in full, and before it nothing had been decided.
func RecoverSwap(live string) (string, error) {
	old, staged := live+SupersededSuffix, live+StagedSuffix
	if !exists(old) && !exists(staged) {
		return "no restore swap was in progress", nil
	}
	switch {
	case exists(live) && exists(old):
		// The second rename landed; only the cleanup was lost.
		if err := os.RemoveAll(old); err != nil {
			return "", fmt.Errorf("remove the superseded world: %w", err)
		}
		return "completed a restore that had already swapped", nil

	case exists(live):
		// Nothing was renamed. The world on disk is the one that was always there.
		if err := os.RemoveAll(staged); err != nil {
			return "", fmt.Errorf("remove the staged world: %w", err)
		}
		return "discarded a restore that had not started swapping", nil

	case exists(staged):
		// Between the renames. The staged world is complete, the live name is empty.
		if err := os.Rename(staged, live); err != nil {
			return "", fmt.Errorf("publish the restored world: %w", err)
		}
		if err := os.RemoveAll(old); err != nil {
			return "", fmt.Errorf("remove the superseded world: %w", err)
		}
		return "completed a restore interrupted between its two renames", nil

	default:
		// The first rename landed and the staged world is gone. Put back what was displaced.
		if err := os.Rename(old, live); err != nil {
			return "", fmt.Errorf("put the previous world back: %w", err)
		}
		return "reversed a restore that lost its staged world", nil
	}
}

func exists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}
