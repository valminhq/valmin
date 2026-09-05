// Package extract unpacks a third-party Thunderstore zip into a staging directory with no
// path, mode or size outcome that the archive itself chooses (B5, 03 §6.5). It imports
// neither store nor api, so it is a pure function over a filesystem (CLAUDE.md §5).
//
// Specification: 03 §6.5, 03 §6.3.
package extract

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// Caps on an arbitrary third-party zip (03 §6.5). Package vars so a test can shrink them
// without generating gigabyte-scale fixtures.
var (
	MaxEntries                = 20_000
	MaxTotalUncompressedBytes = uint64(2 << 30)   // 2 GiB
	MaxEntryUncompressedBytes = uint64(512 << 20) // 512 MiB
)

// Modes are internal/mods/fsutil's, never read from the archive (03 §6.5) — the `cp -a`
// reproduced `drwxrwxrwx` on a hand install, which is why every mode here is set by this
// package rather than trusted from the entry.

var (
	// ErrUnsafePath is an entry whose name is absolute or escapes the destination.
	ErrUnsafePath = errors.New("extract: unsafe path")
	// ErrUnsafeType is a symlink or anything else that is not a plain file or a directory —
	// zip has no hardlink of its own, so this is the closest the format gets to one, and
	// "anything that isn't a plain file or a directory" is the safe direction to refuse in.
	ErrUnsafeType = errors.New("extract: unsafe entry type")
	// ErrLimit is a zip that exceeds the entry-count, total-size or per-entry-size cap, or
	// whose actual decompressed bytes exceed what the entry declared.
	ErrLimit = errors.New("extract: archive exceeds safety limits")
)

// Extract unpacks zipPath into destRoot, which must already exist. Every entry's path and type
// is validated in one pass before the write pass begins, so a hostile entry anywhere in the
// archive aborts with nothing written rather than leaving earlier entries on disk. Every file and
// directory gets a mode this package chooses, never one the archive claims (03 §6.5). The caller
// stages into a fresh, disposable directory, so a rejected archive is simply discarded.
func Extract(zipPath, destRoot string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", zipPath, err)
	}
	defer func() { _ = r.Close() }()

	destRoot, err = filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("resolve destination: %w", err)
	}

	if err := checkLimits(r.File); err != nil {
		return err
	}

	dests := make([]string, len(r.File))
	for i, f := range r.File {
		dest, err := safeJoin(destRoot, f.Name)
		if err != nil {
			return err
		}
		if err := rejectUnsafeType(f); err != nil {
			return err
		}
		dests[i] = dest
	}

	for i, f := range r.File {
		if err := materialize(f, dests[i]); err != nil {
			return err
		}
	}
	return nil
}

// materialize creates dest as a directory or writes it as a regular file, per f's own
// entry type — already proven safe by the validation pass in Extract.
func materialize(f *zip.File, dest string) error {
	if f.FileInfo().IsDir() {
		if err := fsutil.MkdirAllExact(dest); err != nil {
			return fmt.Errorf("create directory %s: %w", f.Name, err)
		}
		return nil
	}
	if err := fsutil.MkdirAllExact(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("create parent of %s: %w", f.Name, err)
	}
	return writeEntry(f, dest)
}

func checkLimits(files []*zip.File) error {
	if len(files) > MaxEntries {
		return fmt.Errorf("%w: %d entries exceeds the %d-entry cap", ErrLimit, len(files), MaxEntries)
	}
	var total uint64
	for _, f := range files {
		if f.UncompressedSize64 > MaxEntryUncompressedBytes {
			return fmt.Errorf("%w: %s is %d bytes, over the %d-byte per-entry cap",
				ErrLimit, f.Name, f.UncompressedSize64, MaxEntryUncompressedBytes)
		}
		total += f.UncompressedSize64
		if total > MaxTotalUncompressedBytes {
			return fmt.Errorf("%w: total uncompressed size exceeds the %d-byte cap",
				ErrLimit, MaxTotalUncompressedBytes)
		}
	}
	return nil
}

// writeEntry copies exactly f.UncompressedSize64 bytes and errors on either side of that: fewer
// is a truncated stream, more is a lied-about size, the zip-bomb shape checkLimits' totals alone
// cannot catch since they are computed from the same lie.
func writeEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open entry %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.OpenFile( //nolint:gosec // dest validated by safeJoin
		dest,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		fsutil.FileMode,
	)
	if err != nil {
		return fmt.Errorf("create %s: %w", f.Name, err)
	}
	defer func() { _ = out.Close() }()

	// checkLimits already refused anything over MaxEntryUncompressedBytes, so this bound is
	// always well inside int64 — restated here rather than trusted silently, since it is
	// what makes the two conversions below provably safe rather than merely believed to be.
	if f.UncompressedSize64 >= math.MaxInt64 {
		return fmt.Errorf("%w: %s declares an unrepresentable size", ErrLimit, f.Name)
	}
	limit := int64(f.UncompressedSize64) + 1
	n, copyErr := io.CopyN(out, rc, limit)
	switch {
	case copyErr == nil:
		return fmt.Errorf("%w: %s decompressed past its declared size", ErrLimit, f.Name)
	case !errors.Is(copyErr, io.EOF):
		return fmt.Errorf("write %s: %w", f.Name, copyErr)
	case uint64(n) != f.UncompressedSize64: //nolint:gosec // n is CopyN's count, always in [0, limit]
		return fmt.Errorf("%w: %s wrote %d bytes, declared %d", ErrLimit, f.Name, n, f.UncompressedSize64)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", f.Name, err)
	}
	// OpenFile's mode is filtered by umask; Chmod after the fact makes it exact regardless.
	if err := os.Chmod(dest, fsutil.FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	return nil
}

// safeJoin normalises name and refuses anything that is absolute or that resolves outside
// destRoot. Normalisation runs first, since a Windows-built zip stores backslash-separated
// names that a check running before normalising would treat as one harmless filename.
func safeJoin(destRoot, rawName string) (string, error) {
	name := strings.ReplaceAll(rawName, `\`, "/")
	if name == "" {
		return "", fmt.Errorf("%w: empty entry name", ErrUnsafePath)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: %q is an absolute path", ErrUnsafePath, rawName)
	}
	if len(name) >= 2 && name[1] == ':' { // a Windows drive letter, e.g. "C:/x"
		return "", fmt.Errorf("%w: %q is an absolute path", ErrUnsafePath, rawName)
	}

	dest := filepath.Clean(filepath.Join(destRoot, filepath.FromSlash(name)))
	if dest != destRoot && !strings.HasPrefix(dest, destRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the destination", ErrUnsafePath, rawName)
	}
	return dest, nil
}

// Unix file-type bits as packed into the upper 16 bits of a zip.FileHeader's ExternalAttrs
// by a Unix-built archive (APPNOTE.TXT §4.4.2.2) — S_IFMT and the two types this package
// accepts.
const (
	unixIFMT  = 0xF000
	unixIFREG = 0x8000
	unixIFDIR = 0x4000
)

// creatorUnix is the high byte of CreatorVersion that marks ExternalAttrs' upper 16 bits as a
// real Unix mode. A Windows-built zip carries none and is trusted as regular-or-directory by its
// name, which is safe since it has no way to encode a symlink.
const creatorUnix = 3

func rejectUnsafeType(f *zip.File) error {
	if f.CreatorVersion>>8 != creatorUnix {
		return nil
	}
	switch f.ExternalAttrs >> 16 & unixIFMT {
	case 0, unixIFREG, unixIFDIR:
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrUnsafeType, f.Name)
	}
}
