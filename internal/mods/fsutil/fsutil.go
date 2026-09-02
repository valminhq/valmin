// Package fsutil holds the small filesystem primitives every internal/mods/* package
// needs and would otherwise each duplicate: the directory mode 08 §2.1 requires and the
// exact-mode Mkdir that makes it stick regardless of the process umask. Extracted from
// internal/mods/extract at WP-M2-04, when internal/mods/cache needed the identical logic
// a second time.
package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirMode/FileMode are 08 §2.1: instance directories are 2775 with setgid so files
// created inside inherit the panel's group; combined with umask 002 that is what makes a
// file land 0664.
//
// `↯` DirMode is built from fs.ModeSetgid, not the numeric literal 0o2775: Go's os.Mkdir
// and os.Chmod translate a FileMode to a raw Unix mode via its own Perm() (the low 9 bits)
// plus its named special-bit flags (ModeSetuid/ModeSetgid/ModeSticky, which live at
// entirely different bit positions than Unix's 04000/02000/01000). Passing 0o2775 as a raw
// numeric FileMode silently drops the setgid bit — Perm() masks it away and no named flag
// is set — and the directory comes out plain 0775, or less once umask reduces it further.
const (
	DirMode              = fs.ModeSetgid | 0o775
	FileMode fs.FileMode = 0o664
)

// MkdirAllExact is os.MkdirAll with one difference: every directory it creates is chmod'd
// to DirMode afterward, so the permission bits are exact regardless of the process
// umask — os.MkdirAll alone only passes DirMode to the mkdir syscall, which the umask
// still filters on every level it creates.
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

// WriteFileAtomic publishes data at path: a temp file in path's own directory, fsynced,
// chmod'd to FileMode, then renamed. 06 §4 requires this discipline of every write the
// panel makes — a torn write leaves a file that exists and is wrong, which is worse than
// one that does not exist.
//
// It reads the whole payload from memory, so it is for small files. Anything the size of a
// game asset streams instead (internal/mods/installer's copyFile).
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
