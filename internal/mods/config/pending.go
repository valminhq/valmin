package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// PendingSuffix names the copy of a config the panel wrote while its server was running. A
// plugin that saves its settings at shutdown writes back the values it loaded at startup, so
// SettlePending puts the panel's values back once the server has stopped.
const PendingSuffix = ".pending"

// SettlePending reapplies every pending copy in the config directory rel under serverDir to
// the config it belongs to, then removes the copy. Only values are carried over: a setting the
// plugin added or dropped stays as the plugin left it. A missing directory has nothing
// pending.
//
// Every path is resolved inside serverDir, which the game server can write: a symlink it
// plants, the config directory included, cannot lead a read or write outside it.
func SettlePending(serverDir, rel string) error {
	server, err := os.OpenRoot(serverDir)
	if err != nil {
		return fmt.Errorf("open %s: %w", serverDir, err)
	}
	defer func() { _ = server.Close() }()
	root, err := server.OpenRoot(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = root.Close() }()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("list %s: %w", rel, err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".cfg"+PendingSuffix) {
			continue
		}
		if err := settleOne(root, name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func settleOne(root *os.Root, pendingName string) error {
	name := strings.TrimSuffix(pendingName, PendingSuffix)
	want, err := readRegular(root, pendingName)
	if err != nil {
		return err
	}
	var next []byte
	have, err := readRegular(root, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		next = want
	case err != nil:
		return err
	default:
		next = Overlay(have, want)
	}
	if !bytes.Equal(next, have) {
		if err := writeAtomicIn(root, name, next); err != nil {
			return fmt.Errorf("settle %s: %w", name, err)
		}
	}
	if err := root.Remove(pendingName); err != nil {
		return fmt.Errorf("remove %s: %w", pendingName, err)
	}
	return nil
}

// writeAtomicIn is fsutil.WriteFileAtomic inside root: a temp file beside name, fsynced,
// chmod'd, then renamed over it. The rename replaces a symlink at name rather than following it.
func writeAtomicIn(root *os.Root, name string, data []byte) error {
	tmp := fmt.Sprintf(".valmin-%d-%s", os.Getpid(), name)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsutil.FileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := root.Chmod(tmp, fsutil.FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, name); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// readRegular reads a regular file inside root. O_NONBLOCK keeps a named pipe planted under
// the name from blocking the open; the mode check then refuses it.
func readRegular(root *os.Root, name string) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return raw, nil
}

// Overlay returns base with every value over also sets, for the settings both contain.
// Everything else in base, comments and layout included, is kept.
func Overlay(base, over []byte) []byte {
	doc := Parse(base)
	for _, s := range Parse(over).Settings() {
		if v, ok := doc.Get(s.Section, s.Key); ok && v != s.Value {
			// Cannot fail: the setting exists and a parsed value holds no newline.
			_ = doc.Set(s.Section, s.Key, s.Value)
		}
	}
	return doc.Bytes()
}
