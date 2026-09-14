package instance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
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

// WorldPath joins name onto the instance's worlds/ and refuses anything that lands outside it
// (B5). Every read and write below goes through it.
//
// filepath.Join cleans as it joins, so the prefix comparison, run after a "../" is already
// resolved away, is what catches an escape; scanning for ".." literally misses "a/../../b".
//
// This does not resolve symlinks: a symlink already inside worlds/ pointing out of it would
// satisfy the check. The threat here is a user-supplied name; the archive-entry half of B5
// belongs to extraction.
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

// WriteWorldFile publishes an in-memory file through the audited worlds/ write boundary (B4).
//
// Atomic by temp-file-then-rename, with the temp file in the same directory as its target, since
// a rename is only atomic within one filesystem. fsync before the rename makes the durability
// claim real: without it the rename can land while the bytes have not.
//
// A crash strictly between the fsync and the rename leaves the temp file behind, named with a
// leading dot and a random suffix so it is never mistaken for the file it would have become.
func WriteWorldFile(dataDir, name string, data []byte) error {
	return WriteWorldFileFromReader(dataDir, name, bytes.NewReader(data))
}

// WriteWorldFileFromReader streams a file through the audited worlds/ write boundary (B4).
func WriteWorldFileFromReader(dataDir, name string, src io.Reader) error {
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

	if _, err := io.Copy(f, src); err != nil {
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

// World is one world the panel can see under an instance's savedir: a `.db`/`.fwl` pair
// sharing a basename, which is what `-world` names (03 §1.3, 03 §4).
//
// A size of -1 is a file that is not there. Half a pair is reported rather than hidden: it is
// the shape a failed import or a hand-copied world leaves behind, and a caller that dropped it
// would answer "no worlds" for a directory that plainly has one.
type World struct {
	Name string
	// Dir is where the pair sits relative to worlds/, so a world the game wrote somewhere the
	// panel does not expect is visible as that rather than as missing. "" is worlds/ itself.
	Dir        string
	DBBytes    int64
	FWLBytes   int64
	ModifiedAt time.Time
}

// recordWorldFile folds one world file into the set being built, creating the world it belongs
// to on first sight of either half.
func recordWorldFile(byKey map[string]*World, root, path, ext string, fi os.FileInfo) error {
	dir, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("locate %s: %w", path, err)
	}
	if dir == "." {
		dir = ""
	}
	name := strings.TrimSuffix(filepath.Base(path), ext)
	world := byKey[dir+"/"+name]
	if world == nil {
		world = &World{Name: name, Dir: dir, DBBytes: -1, FWLBytes: -1}
		byKey[dir+"/"+name] = world
	}
	if ext == ".db" {
		world.DBBytes = fi.Size()
	} else {
		world.FWLBytes = fi.Size()
	}
	if fi.ModTime().After(world.ModifiedAt) {
		world.ModifiedAt = fi.ModTime()
	}
	return nil
}

// Loadable reports whether this is a world a server could be pointed at.
func (w World) Loadable() bool { return w.DBBytes >= 0 && w.FWLBytes >= 0 }

// ListWorlds reports every world under the instance's savedir, sorted by location and name.
//
// It walks the whole tree rather than just worlds_local/, because that is the tree a backup
// archives and the basename is what a backup's verification matches on (02 §4.4 step 5): a
// listing that looked in fewer places than the archive does could call a world missing that
// the archive holds, or the reverse. A savedir that does not exist yet is no worlds and no
// error — an instance that has never run has never written one.
func ListWorlds(dataDir string) ([]World, error) {
	root := WorldsDir(dataDir)
	byKey := map[string]*World{}
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if ext := filepath.Ext(path); ext == ".db" || ext == ".fwl" {
			return recordWorldFile(byKey, root, path, ext, fi)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return []World{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the worlds under %s: %w", root, err)
	}

	worlds := make([]World, 0, len(byKey))
	for _, w := range byKey {
		worlds = append(worlds, *w)
	}
	slices.SortFunc(worlds, func(a, b World) int {
		if a.Dir != b.Dir {
			return strings.Compare(a.Dir, b.Dir)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return worlds, nil
}
