package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// worldFileMode is 08 §2.1's umask 002 as an explicit mode: group-writable, so the setgid
// host group ADR-006 relies on can still read and edit what the panel writes. os.CreateTemp
// makes files 0600, so this is applied before the rename rather than left to chance.
const worldFileMode = 0o664

// WorldsDir is the instance's savedir root — bound at /opt/valheim/worlds and passed as
// -savedir (08 §5). Every world file and all three of 03 §4's player lists live under it.
func WorldsDir(dataDir string) string { return filepath.Join(dataDir, "worlds") }

// ErrOutsideWorlds reports a name that would resolve outside the instance's worlds/.
var ErrOutsideWorlds = errors.New("path escapes the instance's worlds directory")

// WorldPath joins name onto the instance's worlds/ and refuses anything that lands outside
// it (B5). Every read and write below goes through it, so a caller cannot reach the
// filesystem without being checked.
//
// filepath.Join cleans as it joins, so by the time this compares, a "../" has already
// been resolved away — the prefix comparison is what catches an escape, not a scan for the
// literal characters. Scanning for ".." is the version that misses "a/../../b".
//
// This does not resolve symlinks. A symlink already inside worlds/ pointing out of it
// would satisfy the check; the threat here is a user-supplied name, and the archive-entry
// half of B5 (zip-slip, symlink entries) belongs to extraction, where the archive is
// what supplies the link.
func WorldPath(dataDir, name string) (string, error) {
	root := WorldsDir(dataDir)
	joined := filepath.Join(root, name)
	if joined == root || !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%q: %w", name, ErrOutsideWorlds)
	}
	return joined, nil
}

// ReadWorldFile reads one file under worlds/. A file that does not exist is (nil, nil): the
// game creates none of 03 §4's lists until something writes one, and an absent list means
// the same thing as an empty one.
func ReadWorldFile(dataDir, name string) ([]byte, error) {
	path, err := WorldPath(dataDir, name)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G304: path is not caller-controlled — WorldPath above has already
	// resolved and root-checked it, which is the whole reason every read goes through here.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, nil
}

// WriteWorldFile is the single audited write under worlds/ (B4) — no os.WriteFile anywhere
// else in the panel targets this tree, and worlds_test.go's grep is what keeps it that way.
//
// Atomic by temp-file-then-rename, with the temp file in the same directory as its
// target: a rename is only atomic within one filesystem, and worlds/ is a bind mount whose
// backing store is not the panel's. fsync before the rename is what makes the durability
// claim real rather than nominal — without it the rename can land while the bytes have not.
//
// A crash strictly between the fsync and the rename leaves the temp file behind. That is the
// intended outcome: it is named with a leading dot and a random suffix, so it is never
// mistaken for the file it would have become, and the original is untouched.
func WriteWorldFile(dataDir, name string, data []byte) error {
	path, err := WorldPath(dataDir, name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, instanceDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, ".valmin-*")
	if err != nil {
		return fmt.Errorf("stage write of %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp) // no-op once the rename below has succeeded
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", name, err)
	}
	if err := f.Chmod(worldFileMode); err != nil {
		return fmt.Errorf("set mode on %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}
