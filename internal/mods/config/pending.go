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

// SettlePending reapplies every pending copy in the config directory root to the config it
// belongs to, then removes the copy. Only values are carried over: a setting the plugin added
// or dropped stays as the plugin left it. Every read and write stays inside root.
func SettlePending(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("list config directory: %w", err)
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
		if err := fsutil.WriteFileAtomicIn(root, name, next); err != nil {
			return fmt.Errorf("settle %s: %w", name, err)
		}
	}
	if err := root.Remove(pendingName); err != nil {
		return fmt.Errorf("remove %s: %w", pendingName, err)
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
