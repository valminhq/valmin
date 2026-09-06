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

// Backup copies every destination these changes would displace into backupDir, writing nothing
// into serverRoot.
//
// It is separate from Apply and runs over the whole closure before the first file of the first
// package moves (12 §9.4, ADR-009). Rollback reads "a manifest path with no backup" as its own
// to delete, which holds only if everything the install could displace was saved first.
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
// keeping the same relative layout. A path with nothing at it is skipped, which Rollback reads
// as the file being the job's own to delete.
//
// It is what an uninstall saves before removing anything, and what Backup is built from.
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

// Remove deletes each path from serverRoot. The paths come from a package's file manifest and
// from nowhere else (B9): re-running the placement heuristics would describe the package as it
// is today, not as it was when installed.
//
// A path that is already gone is not an error, since a manifest names what the install intended
// to place. Every path is attempted even after one fails, so a stuck file does not strand the
// rest. Directories are left behind: the manifest has no shape for one (04 §2), so removing an
// empty directory would be a guess about whose it was.
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

// Rollback returns serverRoot to its pre-Apply state: every manifest path whose original was
// backed up is restored, and every other manifest path is removed. It runs after a failed apply
// and from the crash sweep, so manifest paths are re-validated rather than trusted: they arrive
// from a database column and this is a privileged process deleting files.
//
// Every path is attempted even after one fails, so a half-applied install is not left on disk
// with nothing that knows about it.
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

// removeStaleTemps deletes the half-written files a killed Apply left behind. No manifest names
// them: copyFile renames a temp file into place, so a process dying in between leaves
// `.valmin-XXXX` beside the file it was about to become.
//
// Only the directories this rollback touched, only regular files, only that prefix. The instance
// lock admits one mod job at a time and the server is stopped (B11), so such a file is this
// job's.
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

// copyFile publishes src at dest atomically: a temp file in dest's own directory, fsynced, then
// renamed (ADR-075), so a killed copy is never visible under the real name. Modes are fsutil's,
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

	return publishFile(in, dest)
}

func publishFile(in io.Reader, dest string) error {
	if err := fsutil.MkdirAllExact(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("create destination: %w", err)
	}

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
