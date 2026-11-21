package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

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
		if isPending(e) {
			errs = append(errs, settleOne(root, e.Name()))
		}
	}
	return errors.Join(errs...)
}

// maxPendingDepth bounds how many directories below root SettlePendingTree looks.
const maxPendingDepth = 8

// SettlePendingTree is SettlePending over root and its subdirectories, at most maxPendingDepth
// deep. Symlinked directories are not followed.
func SettlePendingTree(root *os.Root) error {
	var errs []error
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			errs = append(errs, err)
		case d.IsDir() && strings.Count(p, "/") >= maxPendingDepth:
			return fs.SkipDir
		case isPending(d):
			errs = append(errs, settleOne(root, p))
		}
		return nil
	})
	return errors.Join(append(errs, err)...)
}

// isPending reports whether e is a regular file holding the pending copy of a .cfg.
func isPending(e fs.DirEntry) bool {
	return e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".cfg"+PendingSuffix)
}

// DropPending removes the pending copy of name, if there is one. A writer that replaces a
// config while its server is down calls it, so the next settle can't put an older edit back.
func DropPending(root *os.Root, name string) error {
	if err := root.Remove(name + PendingSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name+PendingSuffix, err)
	}
	return nil
}

func settleOne(root *os.Root, pendingName string) error {
	name := strings.TrimSuffix(pendingName, PendingSuffix)
	want, _, err := fsutil.ReadRegularIn(root, pendingName)
	if err != nil {
		return fmt.Errorf("settle %s: %w", name, err)
	}
	var next []byte
	have, _, err := fsutil.ReadRegularIn(root, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		next = want
	case err != nil:
		return fmt.Errorf("settle %s: %w", name, err)
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
