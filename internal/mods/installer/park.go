package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Disabling a package without uninstalling it (Q37) moves its files out of the server root,
// where BepInEx would load them, into a parking tree the game container never mounts. Nothing
// is deleted: enabling moves the same bytes back to the same paths, and the manifest's Parked
// flag records which paths moved, so an uninstall still removes exactly what the install placed
// (B9) from wherever each file now is.
//
// Every move is a copy, fsynced, then a removal of the original, never a rename across the two
// trees: the server root is game-writable, so it is only ever touched through an os.Root, and
// a rename cannot span two roots. A move interrupted between the two steps leaves the file in
// both places, which Settle resolves.

// ErrParkConflict is a path holding different bytes in the server root and in the parking tree.
// Neither copy is the panel's to discard: something other than the panel wrote one of them.
var ErrParkConflict = errors.New("a different file already exists at this path")

// Movable is what disabling a package moves: every file outside BepInEx/config/. A config
// file is the admin's (its edits survive installs and uninstalls alike), nothing loads it
// while its plugin is parked, and leaving it keeps it editable.
func Movable(manifest []ManifestEntry) []string {
	out := make([]string, 0, len(manifest))
	for _, e := range manifest {
		if !e.Parked && !UserConfig(e.Path) {
			out = append(out, e.Path)
		}
	}
	return out
}

// ParkedPaths is the manifest's parked paths.
func ParkedPaths(manifest []ManifestEntry) []string {
	out := make([]string, 0, len(manifest))
	for _, e := range manifest {
		if e.Parked {
			out = append(out, e.Path)
		}
	}
	return out
}

// Split separates a manifest by where its files are: the server root, or the parking tree.
func Split(manifest []ManifestEntry) (inServer, parked []ManifestEntry) {
	for _, e := range manifest {
		if e.Parked {
			parked = append(parked, e)
		} else {
			inServer = append(inServer, e)
		}
	}
	return inServer, parked
}

// MarkParked returns manifest with Parked set on exactly the given paths.
func MarkParked(manifest []ManifestEntry, paths []string) []ManifestEntry {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	out := make([]ManifestEntry, len(manifest))
	for i, e := range manifest {
		e.Parked = set[e.Path]
		out[i] = e
	}
	return out
}

// Park moves each path from serverRoot to the same relative path under parkDir and reports the
// paths it moved. A path with nothing at it is not moved and not reported: the flag must name
// only files the parking tree actually holds. On an error the paths already moved are still
// reported, so the caller can move them back.
func Park(paths []string, serverRoot, parkDir string) (moved []string, err error) {
	root, err := openServerRoot(serverRoot, false)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, nil
	}
	defer func() { _ = root.Close() }()

	for _, p := range paths {
		rel, err := checkDest(p)
		if err != nil {
			return moved, err
		}
		ok, err := parkOne(root, rel, filepath.Join(parkDir, rel))
		if err != nil {
			return moved, fmt.Errorf("park %s: %w", p, err)
		}
		if ok {
			moved = append(moved, p)
		}
	}
	return moved, nil
}

// parkOne copies rel out of root to dest and removes it from root. ok is false when root holds
// nothing at rel.
func parkOne(root *os.Root, rel, dest string) (ok bool, err error) {
	// O_NONBLOCK and the regular-file check in backupOne: a game process can plant a named pipe
	// at a manifest path, as BackupPaths documents.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open: %w", err)
	}
	err = backupOne(f, dest)
	_ = f.Close()
	if err != nil {
		return false, err
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove the original: %w", err)
	}
	return true, nil
}

// Unpark moves each path from parkDir back to the same relative path under serverRoot and
// reports the paths it moved. A path already back in serverRoot with the parked bytes, as an
// interrupted earlier attempt leaves it, only has its parked copy removed. One holding other
// bytes is refused with ErrParkConflict rather than overwritten. A path parkDir does not hold
// is an error: the manifest says it is there, and enabling a package with a file missing would
// load it broken.
func Unpark(paths []string, parkDir, serverRoot string) (moved []string, err error) {
	root, err := openServerRoot(serverRoot, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	for _, p := range paths {
		rel, err := checkDest(p)
		if err != nil {
			return moved, err
		}
		if err := unparkOne(root, rel, filepath.Join(parkDir, rel)); err != nil {
			return moved, fmt.Errorf("restore %s: %w", p, err)
		}
		moved = append(moved, p)
	}
	return moved, nil
}

func unparkOne(root *os.Root, rel, parked string) error {
	if _, err := os.Lstat(parked); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if present, _ := regularIn(root, rel); present {
				// Already moved back by an attempt that stopped before recording it.
				return nil
			}
			return fmt.Errorf("the parked copy is missing: %w", err)
		}
		return fmt.Errorf("stat the parked copy: %w", err)
	}
	present, err := regularIn(root, rel)
	if err != nil {
		return err
	}
	if present {
		same, err := sameBytes(root, rel, parked)
		if err != nil {
			return err
		}
		if !same {
			return ErrParkConflict
		}
	} else if err := restoreOne(root, rel, parked); err != nil {
		return err
	}
	if err := os.Remove(parked); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the parked copy: %w", err)
	}
	return nil
}

// Settle returns every movable path of a manifest to the place the manifest records for it,
// after a disable or enable that stopped part-way: a crash, or a failure the job is undoing.
// A file found only in the other tree is moved back; one in both trees with the same bytes loses
// the stray copy; one in both with different bytes is left alone and reported, since the panel
// cannot tell which of the two someone meant. Every path is attempted.
func Settle(manifest []ManifestEntry, serverRoot, parkDir string) error {
	root, err := openServerRoot(serverRoot, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var errs []error
	for _, e := range manifest {
		if UserConfig(e.Path) {
			continue
		}
		if err := settleOne(root, e, parkDir); err != nil {
			errs = append(errs, fmt.Errorf("settle %s: %w", e.Path, err))
		}
	}
	return errors.Join(errs...)
}

func settleOne(root *os.Root, e ManifestEntry, parkDir string) error {
	rel, err := checkDest(e.Path)
	if err != nil {
		return err
	}
	parked := filepath.Join(parkDir, rel)
	inParking := true
	if _, err := os.Lstat(parked); errors.Is(err, os.ErrNotExist) {
		inParking = false
	} else if err != nil {
		return fmt.Errorf("stat the parked copy: %w", err)
	}

	if !e.Parked {
		if !inParking {
			return nil
		}
		// unparkOne restores a copy the server root lacks, drops one duplicating it, and
		// refuses one that differs.
		return unparkOne(root, rel, parked)
	}

	inServer, err := regularIn(root, rel)
	if err != nil || !inServer {
		return err
	}
	if !inParking {
		_, err := parkOne(root, rel, parked)
		return err
	}
	same, err := sameBytes(root, rel, parked)
	if err != nil {
		return err
	}
	if !same {
		return ErrParkConflict
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the stray copy: %w", err)
	}
	return nil
}

// regularIn reports whether root holds a regular file at rel.
func regularIn(root *os.Root, rel string) (bool, error) {
	info, err := root.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s: %w", rel, ErrUnsupportedEntry)
	}
	return true, nil
}

// sameBytes compares the file at rel inside root with the one at other.
func sameBytes(root *os.Root, rel, other string) (bool, error) {
	a, err := hashIn(root, rel)
	if err != nil {
		return false, err
	}
	b, err := sha256File(other)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func hashIn(root *os.Root, rel string) (string, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
