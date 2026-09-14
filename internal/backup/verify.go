package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
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
	// Directory is 1.0's layout: the world is a directory of `_main.<gen>.*` files and chunks
	// rather than a `.db`/`.fwl` pair (03 §4).
	Directory bool
}

// dataName and headerName are the two halves as the world's own layout spells them, so an
// error names a file an operator can go and look for.
func (v Verified) dataName() string {
	if v.Directory {
		return v.WorldName + "/_main.<n>.db2"
	}
	return v.WorldName + ".db"
}

func (v Verified) headerName() string {
	if v.Directory {
		return v.WorldName + "/_main.<n>.fwl2"
	}
	return v.WorldName + ".fwl"
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

	// Every world the archive holds, in either layout. The scan is what answers both
	// questions: whether this world is here, and — when it is not — what is (ADR-179).
	scan := WorldScan{}
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
		// Matched wherever in the archive it sits, since the archive's shape is the savedir's
		// and a world is identified by its name rather than its depth. The game's own
		// _backup_auto-* saves are named after the world and sit beside it; they classify as
		// their own world and so cannot satisfy this one's pair check (03 §4.1 rule 5).
		scan.Add(hdr.Name, hdr.Size)
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

	return verified(scan, worldName)
}

// verified applies the pair rule and the size floors to what the walk found.
func verified(scan WorldScan, worldName string) (Verified, error) {
	found := Verified{WorldName: worldName, DBBytes: -1, FWLBytes: -1}
	world := scan[worldName]
	if world != nil {
		found.DBBytes, found.FWLBytes = world.DataBytes, world.HeaderBytes
		found.Directory = world.Directory
	}
	if err := verifyPair(found, worldName, scan); err != nil {
		return Verified{}, err
	}
	return found, nil
}

// heldInstead describes what worlds the archive does carry, for an error about the one it does
// not. An empty list is its own answer and the more serious one: the worlds tree the archive
// was taken from held nothing the game would load.
func heldInstead(scan WorldScan) string {
	names := make([]string, 0, len(scan))
	for name := range scan {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "; it holds no world at all"
	}
	slices.Sort(names)
	if len(names) > maxWorldsNamed {
		return fmt.Sprintf("; it holds %s and %d more",
			strings.Join(names[:maxWorldsNamed], ", "), len(names)-maxWorldsNamed)
	}
	return "; it holds " + strings.Join(names, ", ")
}

// verifyPair applies the pair rule and the size floors to what the walk found. scan is every
// world the archive carries, which is what the missing-world error reports back.
func verifyPair(found Verified, worldName string, scan WorldScan) error {
	if !scan.Complete(worldName) {
		return fmt.Errorf("%w: %s%s", ErrWorldMissing, missingHalves(found, worldName),
			heldInstead(scan))
	}

	dataFloor, headerFloor := int64(minDBBytes), int64(minFWLBytes)
	if found.Directory {
		// The pre-1.0 floors were measured against files that held the whole world. A `.db2`
		// sits beside chunk files that hold most of it, and no floor for it has been measured
		// (Q56), so the only claim made here is that neither half is empty.
		dataFloor, headerFloor = 1, 1
	}
	if found.DBBytes < dataFloor {
		return fmt.Errorf("%w: %s is %d bytes",
			ErrWorldImplausible, found.dataName(), found.DBBytes)
	}
	if found.FWLBytes < headerFloor {
		return fmt.Errorf("%w: %s is %d bytes",
			ErrWorldImplausible, found.headerName(), found.FWLBytes)
	}
	return nil
}

// missingHalves names what was not there, in the layout's own vocabulary.
func missingHalves(found Verified, worldName string) string {
	var missing []string
	if found.DBBytes < 0 {
		missing = append(missing, found.dataName())
	}
	if found.FWLBytes < 0 {
		missing = append(missing, found.headerName())
	}
	if len(missing) == 0 {
		return worldName
	}
	return strings.Join(missing, " and ")
}
