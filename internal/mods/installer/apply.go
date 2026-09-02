package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	return BackupPaths(DestPaths(changes), serverRoot, backupDir)
}

// DestPaths is where these changes would write. A skipped change is not one of them —
// nothing is written there, so there is nothing to save and nothing to undo.
func DestPaths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Action != ActionSkip {
			out = append(out, c.Dest)
		}
	}
	return out
}

// BackupPaths saves whatever currently lives at each path under serverRoot into backupDir,
// keeping the same relative layout. A path with nothing at it is skipped, and Rollback
// reads that absence as "this file is the job's, delete it".
//
// It is what an uninstall saves before it removes anything, and what Backup is built from:
// the two operations displace files for different reasons and undo them the same way.
func BackupPaths(paths []string, serverRoot, backupDir string) error {
	for _, p := range paths {
		if err := checkDest(p); err != nil {
			return err
		}
		src := filepath.Join(serverRoot, filepath.FromSlash(p))
		switch _, err := os.Lstat(src); {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if err := copyFile(src, filepath.Join(backupDir, filepath.FromSlash(p))); err != nil {
			return fmt.Errorf("back up %s: %w", p, err)
		}
	}
	return nil
}

// Remove deletes each path from serverRoot. `↯` The paths come from a package's file
// manifest and from nowhere else (B9): re-running the placement heuristics to work out what
// to delete would be a guess about a package as it is *today*, against a server that has
// whatever it shipped on the day it was installed.
//
// A path that is already gone is not an error — a manifest written before the files moved
// names what the install *intended* to place, and after a partial rollback some of it is
// not there. Every path is attempted even after one fails, so a single stuck file leaves
// the rest removable rather than half of the package on disk with an error and no record.
//
// Directories are left behind. 04 §2's manifest is `[{path, sha256}]` and has no shape for
// a directory, so no directory in server/ is owned by anyone; the empty ones an install
// created are inert, and two packages sharing BepInEx/plugins/ makes deleting them on
// emptiness a guess about whose it was.
func Remove(paths []string, serverRoot string) error {
	var errs []error
	for _, p := range paths {
		if err := checkDest(p); err != nil {
			errs = append(errs, err)
			continue
		}
		dest := filepath.Join(serverRoot, filepath.FromSlash(p))
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// Paths is a manifest's destinations, for the two operations that care about where a
// package's files are and not what is in them.
func Paths(manifest []ManifestEntry) []string {
	out := make([]string, 0, len(manifest))
	for _, e := range manifest {
		out = append(out, e.Path)
	}
	return out
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
	touched := map[string]bool{}
	for _, e := range manifest {
		if err := checkDest(e.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		dest := filepath.Join(serverRoot, filepath.FromSlash(e.Path))
		touched[filepath.Dir(dest)] = true
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
	return errors.Join(append(errs, removeStaleTemps(touched))...)
}

// tempPrefix is what an in-flight copyFile is called before it is renamed into place.
const tempPrefix = ".valmin-"

// removeStaleTemps deletes the half-written files a killed Apply left behind.
//
// `↯` No manifest names them. copyFile writes to a temp file in the destination's own
// directory and renames it, so a process that dies in between leaves `.valmin-XXXX` sitting
// beside the file it was about to become — and a rollback that walked manifest paths alone
// returned a tree that was *nearly* byte-identical, which is not a claim ADR-009 can round
// down. Found by WP-M2-12's AT-M2-4 on its first run, against the real binary.
//
// Only the directories this rollback touched, only regular files, only that prefix. Nothing
// else can be writing here: the instance lock admits one mod job at a time and B11 keeps the
// server stopped, so a temp file under a directory this manifest names is this job's.
func removeStaleTemps(dirs map[string]bool) error {
	var errs []error
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("scan %s for interrupted writes: %w", dir, err))
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), tempPrefix) {
				continue
			}
			p := filepath.Join(dir, entry.Name())
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove the interrupted write %s: %w", p, err))
			}
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

	tmp, err := os.CreateTemp(filepath.Dir(dest), tempPrefix+"*")
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
