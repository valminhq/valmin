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

// Caps on an arbitrary third-party zip (03 §6.5). Package vars rather than a config key:
// nothing in the corpus (the largest real package downloaded, Therzie-Warfare, is ~182 MB
// compressed) is within two orders of magnitude of these, and a knob nobody has needed to
// turn is a knob nobody has tested — they are vars only so a test can shrink them rather
// than generate real gigabyte-scale fixtures to exercise the boundary.
var (
	MaxEntries                = 20_000
	MaxTotalUncompressedBytes = uint64(2 << 30)   // 2 GiB
	MaxEntryUncompressedBytes = uint64(512 << 20) // 512 MiB
)

// Modes are internal/mods/fsutil's, never read from the archive (03 §6.5) — M0's `cp -a`
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

// Extract unpacks zipPath into destRoot, which must already exist. Extraction is
// all-or-nothing with respect to safety: every entry's path and type is validated in one
// pass *before* the write pass begins, so one hostile entry anywhere in the archive — not
// only a first entry — aborts with nothing written, rather than leaving whatever legitimate
// entries preceded it on disk. Every written file and directory gets a mode this package
// chooses, never one the archive claims (03 §6.5). The caller stages into a fresh,
// disposable directory, so a rejected archive is simply discarded rather than reconciled.
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

// writeEntry copies exactly f.UncompressedSize64 bytes and errors on either side of that —
// fewer means a truncated stream, more means the declared size lied, which is the
// zip-bomb shape this guards against independent of the totals in checkLimits (an archive
// that lies about one entry's size defeats a total computed from the same lie).
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
// destRoot. Normalisation runs first: a Windows-built zip (Tekla-AutoRepair in the corpus)
// stores backslash-separated names, and a check that ran before normalising would treat
// "..\\..\\etc\\passwd" as one harmless-looking filename and let it through.
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

// creatorUnix is the high byte of CreatorVersion that marks ExternalAttrs' upper 16 bits as
// a real Unix mode. A zip built on Windows (F3's Tekla-AutoRepair) carries none, and is
// trusted as regular-or-directory by its own name — trivially safe, since a Windows zip has
// no way to encode a symlink in the first place.
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
