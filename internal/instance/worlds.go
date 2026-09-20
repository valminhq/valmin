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
	"syscall"
	"time"

	"github.com/valminhq/valmin/internal/backup"
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
//
// The open goes through an os.Root confined to worlds/: WorldPath's check is lexical and
// never touches the filesystem, so a symlink the game process planted there would otherwise be
// followed outside the instance. os.Root refuses that resolution instead.
func ReadWorldFile(dataDir, name string) ([]byte, error) {
	path, err := WorldPath(dataDir, name)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(WorldsDir(dataDir), path)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", name, ErrOutsideWorlds)
	}

	root, err := os.OpenRoot(WorldsDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open worlds directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	// O_NONBLOCK: a game process can plant a named pipe at this name, and opening one for
	// reading blocks until a writer shows up. Without it, one such file would hang whatever
	// goroutine reads it, forever. Harmless on a regular file, which is always ready.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%q: %w", name, ErrOutsideWorlds)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q: %w", name, ErrOutsideWorlds)
	}

	data, err := io.ReadAll(f)
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

// World is one world the panel can see under an instance's savedir, in whichever layout the
// game that wrote it uses (03 §4). A size of -1 is a half that is not there; half a world is
// reported rather than hidden, because it is the shape a failed import leaves behind and a
// caller that dropped it would answer "no worlds" for a directory that plainly has one.
type World struct {
	Name string
	// Dir is where the world sits relative to worlds/, so a world the game wrote somewhere
	// the panel does not expect is visible as that rather than as missing. "" is worlds/.
	Dir string
	// Directory is 1.0's layout: the world is a directory of `_main.<gen>.*` files and chunks
	// rather than a `.db`/`.fwl` pair.
	Directory   bool
	DataBytes   int64
	HeaderBytes int64
	// Bytes is everything the world occupies, which for 1.0 includes the chunk files that
	// hold most of it.
	Bytes      int64
	ModifiedAt time.Time
}

// Loadable reports whether this is a world a server could be pointed at.
func (w *World) Loadable() bool { return w.DataBytes >= 0 && w.HeaderBytes >= 0 }

// RemoveWorld deletes one world from the instance's savedir, in either of 03 §4's layouts.
// A world that is already gone is not an error.
//
// A pair takes its `.old` fallbacks with it: those classify as no world at all, so leaving
// them behind leaves the world's bytes on disk under names nothing lists.
func RemoveWorld(dataDir string, w *World) error {
	path, err := WorldPath(dataDir, filepath.Join(w.Dir, w.Name))
	if err != nil {
		return err
	}
	if w.Directory {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", w.Name, err)
		}
		return nil
	}
	for _, ext := range []string{".db", ".fwl", ".db.old", ".fwl.old"} {
		if err := os.Remove(path + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s%s: %w", w.Name, ext, err)
		}
	}
	return nil
}

// ListWorlds reports every world under the instance's savedir, sorted by location and name.
//
// It walks the whole tree rather than just worlds_local/, because that is the tree a backup
// archives and a world is identified by its name rather than its depth (02 §4.4 step 5): a
// listing that looked in fewer places than the archive does could call a world missing that
// the archive holds, or the reverse. The layout knowledge is backup's, which is where it has
// to live — that package must recognise a world inside a tar, where there is no filesystem to
// ask (ADR-179).
func ListWorlds(dataDir string) ([]World, error) {
	scan, err := backup.ScanWorlds(WorldsDir(dataDir))
	if err != nil {
		return nil, fmt.Errorf("list the worlds of %s: %w", dataDir, err)
	}
	worlds := make([]World, 0, len(scan))
	for name, found := range scan {
		worlds = append(worlds, World{
			Name: name, Dir: found.Dir, Directory: found.Directory,
			DataBytes: found.DataBytes, HeaderBytes: found.HeaderBytes,
			Bytes: found.Bytes, ModifiedAt: found.ModifiedAt,
		})
	}
	slices.SortFunc(worlds, func(a, b World) int {
		if a.Dir != b.Dir {
			return strings.Compare(a.Dir, b.Dir)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return worlds, nil
}
