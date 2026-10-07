// Package fsutil holds the filesystem primitives every internal/mods package shares: the
// directory mode 08 §2.1 requires, and the exact-mode Mkdir that makes it stick regardless of
// the process umask.
package fsutil

import (
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirMode and FileMode are 08 §2.1: instance directories are 2775 with setgid so files created
// inside inherit the panel's group, which with umask 002 lands a file at 0664.
//
// DirMode is built from fs.ModeSetgid rather than the literal 0o2775. Go translates a FileMode
// through Perm() plus its named special-bit flags, whose bit positions differ from Unix's, so a
// raw 0o2775 silently loses the setgid bit and the directory comes out plain 0775.
const (
	DirMode              = fs.ModeSetgid | 0o775
	FileMode fs.FileMode = 0o664
)

// MkdirAllExact is os.MkdirAll with every directory it creates chmod'd to DirMode afterward, so
// the bits are exact regardless of the process umask, which filters the mkdir syscall at every
// level.
func MkdirAllExact(path string) error {
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil
	}
	if err := MkdirAllExact(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, DirMode); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	if err := os.Chmod(path, DirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// WriteFileAtomic publishes data at path: a temp file in path's own directory, fsynced, chmod'd
// to FileMode, then renamed, as 06 §4 requires of every write the panel makes.
//
// It takes the whole payload in memory, so it is for small files; anything the size of a game
// asset streams through internal/mods/installer's copyFile instead.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".valmin-*")
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}

// WriteFileAtomicIn is WriteFileAtomic for a name inside root, which a symlink cannot lead
// out of. The rename replaces a symlink at name rather than following it.
func WriteFileAtomicIn(root *os.Root, name string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(name), ".valmin-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", name, err)
	}
	defer func() {
		_ = f.Close()
		_ = root.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := root.Chmod(tmp, FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, name); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}
