package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

// Size floors below which a world file cannot be a world (03 §4.1 rule 5, 03 §4.2). They
// catch an empty or truncated member, not a small world: the smallest world measured is
// 998 KB, and a .fwl needs nine bytes for its two int32s and a name length prefix.
const (
	minDBBytes  = 1024
	minFWLBytes = 9
)

// maxWorldsNamed bounds how many world files a missing-pair error lists back. The point is to
// say what the archive holds instead, not to reproduce its index.
const maxWorldsNamed = 6

// maxTrailerBytes bounds what Verify will read past the tar's end. Archive writes nothing
// there; other tar writers pad to a blocking factor, which is kilobytes at most.
const maxTrailerBytes = 1 << 20

// Reasons an archive fails verification, as sentinels so a caller can tell them apart and
// report which one refused it.
var (
	// ErrArchiveUnreadable is a file that is not a readable gzipped tar: truncated, corrupt,
	// or never finished being written.
	ErrArchiveUnreadable = errors.New("archive cannot be read")
	// ErrWorldMissing is an archive carrying no .db/.fwl pair for the named world.
	ErrWorldMissing = errors.New("archive lists no world pair")
	// ErrWorldImplausible is a pair whose files are too small to be a world.
	ErrWorldImplausible = errors.New("archive's world files are too small to be a world")
)

// Verified is what an archive was found to contain.
type Verified struct {
	WorldName string
	DBBytes   int64
	FWLBytes  int64
}

// Verify reads a written archive back and reports whether it carries worldName's pair at a
// plausible size (02 §4.4 step 5, B8).
//
// It never compares the .fwl against a previous one: the engine rewrites that file between
// sessions, so such a check fails on every healthy backup (B8, 03 §4).
func Verify(archivePath, worldName string) (Verified, error) {
	// archivePath comes from a catalogue row, never from a request (D13).
	f, err := os.Open(archivePath) //nolint:gosec // see above
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
	}
	defer func() { _ = gz.Close() }()

	found := Verified{WorldName: worldName, DBBytes: -1, FWLBytes: -1}
	// Every world file the archive holds, for the error that says so. A pair that is missing
	// is either a world saved under another name or a worlds tree with nothing in it, and
	// those want opposite things done about them (operator report, 14 Sep 2026).
	var worlds []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Verified{}, fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Matched by basename, since -world names the file stem (03 §1.3): the game's own
		// _backup_auto-* saves share the directory and must not satisfy the pair check.
		base := path.Base(hdr.Name)
		switch base {
		case worldName + ".db":
			found.DBBytes = hdr.Size
		case worldName + ".fwl":
			found.FWLBytes = hdr.Size
		}
		if strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".fwl") {
			worlds = append(worlds, base)
		}
	}

	// The tar reader stops at the two zero blocks, leaving gzip's trailer and its CRC32
	// unread, so a truncated archive parses clean. Draining to the real end verifies it,
	// bounded so the drain cannot itself be a decompression bomb.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailerBytes+1))
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %w", ErrArchiveUnreadable, err)
	}
	if n > maxTrailerBytes {
		return Verified{}, fmt.Errorf(
			"%w: more than %d bytes follow the archive's end", ErrArchiveUnreadable, maxTrailerBytes)
	}

	if err := verifyPair(found, worldName, worlds); err != nil {
		return Verified{}, err
	}
	return found, nil
}

// heldInstead describes what world files the archive does carry, for an error about the one it
// does not. An empty list is its own answer and the more serious one: the worlds tree the
// archive was taken from held nothing the game would load.
func heldInstead(worlds []string) string {
	if len(worlds) == 0 {
		return "; it holds no world file at all"
	}
	slices.Sort(worlds)
	worlds = slices.Compact(worlds)
	if len(worlds) > maxWorldsNamed {
		return fmt.Sprintf("; it holds %s and %d more",
			strings.Join(worlds[:maxWorldsNamed], ", "), len(worlds)-maxWorldsNamed)
	}
	return "; it holds " + strings.Join(worlds, ", ")
}

// verifyPair applies the pair rule and the size floors to what the walk found. worlds is every
// world file the archive carries, which is what the missing-pair error reports back.
func verifyPair(found Verified, worldName string, worlds []string) error {
	var missing []string
	if found.DBBytes < 0 {
		missing = append(missing, worldName+".db")
	}
	if found.FWLBytes < 0 {
		missing = append(missing, worldName+".fwl")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s%s",
			ErrWorldMissing, strings.Join(missing, " and "), heldInstead(worlds))
	}

	if found.DBBytes < minDBBytes {
		return fmt.Errorf("%w: %s.db is %d bytes", ErrWorldImplausible, worldName, found.DBBytes)
	}
	if found.FWLBytes < minFWLBytes {
		return fmt.Errorf("%w: %s.fwl is %d bytes", ErrWorldImplausible, worldName, found.FWLBytes)
	}
	return nil
}
