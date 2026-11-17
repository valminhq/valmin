package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/valminhq/valmin/internal/command"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// TrackConfigBefore records current, the bytes of the config file name inside dir, before a
// write replaces them. A file with no history yet first takes the copies earlier releases kept
// beside it: the original as its baseline, then the copy of what the last save replaced.
// current is then recorded as found when nothing came before it, as a panel save after the
// imported copies, and as changed outside the panel when it differs from the newest revision.
// dir is nil when the directory does not exist yet. A write must not go ahead if this fails.
func TrackConfigBefore(
	ctx context.Context, db *store.DB, dir *os.Root, instanceID, name string, current []byte,
) error {
	if !KeepsHistory(name) {
		return nil
	}
	revs, err := db.ConfigRevisions(ctx, instanceID, name)
	if err != nil {
		return err
	}
	out := make([]store.ConfigRevision, 0, 3)
	if len(revs) == 0 {
		legacy, err := legacyCopies(dir, instanceID, name)
		if err != nil {
			return err
		}
		out = append(out, legacy...)
	}
	source := store.ConfigOutside
	switch {
	case len(revs) == 0 && len(out) == 0:
		source = store.ConfigFound
	case len(revs) == 0:
		// With copies kept, the file on disk is most likely what the last panel save wrote.
		source = store.ConfigEdit
	}
	out = append(out, store.ConfigRevision{InstanceID: instanceID, File: name, Source: source, Content: current})
	if err := db.RecordConfigRevisions(ctx, out); err != nil {
		return fmt.Errorf("record the current %s: %w", name, err)
	}
	return nil
}

// KeepsHistory reports whether the config file name, relative to its directory, has a history.
// The RCON plugin's file has none: it holds the console password the panel mints.
func KeepsHistory(name string) bool { return name != command.ConfigFile }

// legacyCopies reads the `.orig` and `.bak` copies beside name as revisions dated by their
// modification time. A missing copy, anything but a regular file and a copy too large to keep
// are skipped, and a nil dir, a directory that does not exist yet, has none.
func legacyCopies(dir *os.Root, instanceID, name string) ([]store.ConfigRevision, error) {
	var out []store.ConfigRevision
	if dir == nil {
		return nil, nil
	}
	for _, c := range []struct {
		suffix string
		source store.ConfigSource
	}{
		{modconfig.OriginalSuffix, store.ConfigFound},
		{modconfig.BackupSuffix, store.ConfigEdit},
	} {
		raw, info, err := readKeptCopy(dir, name+c.suffix)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		// A copy dated ahead of the clock would stay newest; it is dated now instead.
		at := info.ModTime()
		if now := time.Now(); at.After(now) {
			at = now
		}
		out = append(out, store.ConfigRevision{
			InstanceID: instanceID, File: name, Source: c.source, Content: raw, CreatedAt: at,
		})
	}
	return out, nil
}

// readKeptCopy reads one kept copy, or returns nil bytes when there is none to take: missing,
// not a regular file, or over store.MaxConfigRevisionBytes, which is checked before reading.
func readKeptCopy(dir *os.Root, name string) ([]byte, fs.FileInfo, error) {
	f, info, err := fsutil.OpenRegularIn(dir, name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the kept copy %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if info.Size() > store.MaxConfigRevisionBytes {
		return nil, nil, nil
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, fmt.Errorf("read the kept copy %s: %w", name, err)
	}
	if raw == nil {
		raw = []byte{}
	}
	return raw, info, nil
}
