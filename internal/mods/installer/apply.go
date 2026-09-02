package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// Backup copies every destination these changes would displace into backupDir, and writes
// nothing into serverRoot.
//
// `↯` It is a separate step from Apply, and it runs over the **whole closure** before the
// first file of the first package moves (12 §9.4, ADR-009). Rollback reads "this manifest
// path has no backup" as "this path is ours, delete it" — which is only true if every
// pre-existing file the install was ever going to touch was already saved. Backing up
// per package, inside its own Apply, makes it false: a crash or an earlier package's
// failure leaves later packages' manifests recorded with no backups behind them, and the
// rollback then deletes operator files the install never displaced.
func Backup(changes []Change, serverRoot, backupDir string) error {
	for _, c := range changes {
		if c.Action == ActionSkip {
			continue
		}
		if err := checkDest(c.Dest); err != nil {
			return err
		}
		dest := filepath.Join(serverRoot, filepath.FromSlash(c.Dest))
		switch _, err := os.Lstat(dest); {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("stat %s: %w", c.Dest, err)
		}
		if err := copyFile(dest, filepath.Join(backupDir, filepath.FromSlash(c.Dest))); err != nil {
			return fmt.Errorf("back up %s: %w", c.Dest, err)
		}
	}
	return nil
}

// Apply writes every non-skipped change into serverRoot. Backup must already have run over
// the whole closure — this is the step that is allowed to fail half-way, and Rollback is
// what makes that recoverable.
func Apply(changes []Change, serverRoot string) error {
	for _, c := range changes {
		if c.Action == ActionSkip {
			continue
		}
		if err := checkDest(c.Dest); err != nil {
			return err
		}
		dest := filepath.Join(serverRoot, filepath.FromSlash(c.Dest))
		if err := copyFile(c.Source, dest); err != nil {
			return fmt.Errorf("place %s: %w", c.Dest, err)
		}
	}
	return nil
}

// Rollback returns serverRoot to its pre-Apply state: every manifest path whose original
// was backed up is restored, and every other manifest path is removed. It is what runs
// after a failed apply, and what the crash sweep runs on a job that never finished — which
// is why manifest paths are re-validated here rather than trusted. They arrive from a
// database column, and this is a privileged process deleting files.
//
// Every path is attempted even after one fails: stopping at the first error would leave
// the rest of a half-applied install on disk with nothing left that knows about it.
func Rollback(manifest []ManifestEntry, serverRoot, backupDir string) error {
	var errs []error
	for _, e := range manifest {
		if err := checkDest(e.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		dest := filepath.Join(serverRoot, filepath.FromSlash(e.Path))
		saved := filepath.Join(backupDir, filepath.FromSlash(e.Path))

		switch _, err := os.Lstat(saved); {
		case err == nil:
			if err := copyFile(saved, dest); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", e.Path, err))
			}
			continue
		case !errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("stat backup of %s: %w", e.Path, err))
			continue
		}
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
		}
	}
	return errors.Join(errs...)
}

// copyFile publishes src at dest atomically: a temp file in dest's own directory, fsynced,
// then renamed — the same discipline internal/backup.Archive and the zip cache use
// (ADR-075), so a killed copy is never visible under the real name. Modes are fsutil's,
// never the source's.
func copyFile(src, dest string) error {
	if err := fsutil.MkdirAllExact(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("create the parent of %s: %w", dest, err)
	}
	in, err := os.Open(src) //nolint:gosec // src is a staging path or a backup path this package built
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".valmin-*")
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", dest, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy into %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, fsutil.FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	return nil
}
