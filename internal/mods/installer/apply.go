package installer

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// openServerRoot opens serverRoot for confined access. create controls what happens when it
// does not exist yet: Apply and a Rollback restore may run before anything has touched
// serverRoot, so they create it; Remove and BackupPaths have nothing to act on before the tree
// exists, so a missing root there is "nothing here" rather than an error.
func openServerRoot(serverRoot string, create bool) (*os.Root, error) {
	root, err := os.OpenRoot(serverRoot)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err := fsutil.MkdirAllExact(serverRoot); err != nil {
			return nil, fmt.Errorf("create server root: %w", err)
		}
		root, err = os.OpenRoot(serverRoot)
	}
	if err != nil {
		return nil, fmt.Errorf("open server root: %w", err)
	}
	return root, nil
}

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
//
// Every read goes through an os.Root confined to serverRoot: the game process that owns it can
// plant a symlink at any manifest path, and following one out of the tree would back up, and
// later restore, a file the panel never wrote.
func BackupPaths(paths []string, serverRoot, backupDir string) error {
	root, err := openServerRoot(serverRoot, false)
	if err != nil {
		return err
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}

	for _, p := range paths {
		rel, err := checkDest(p)
		if err != nil {
			return err
		}
		if root == nil {
			// Nothing has ever written to serverRoot, so there is nothing at any path to save.
			continue
		}
		// O_NONBLOCK: a running game process can plant a named pipe at a manifest path, and
		// opening one for reading blocks until a writer connects. backupOne's mode check
		// rejects anything but a regular file before a read is attempted.
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		err = backupOne(f, filepath.Join(backupDir, rel))
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("back up %s: %w", p, err)
		}
	}
	return nil
}

func backupOne(f *os.File, dest string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w", ErrUnsupportedEntry)
	}
	return publishFile(f, dest)
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
	root, err := openServerRoot(serverRoot, false)
	if err != nil {
		return err
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}

	var errs []error
	for _, p := range paths {
		rel, err := checkDest(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if root == nil {
			// Nothing has ever written to serverRoot, so every path is already gone.
			continue
		}
		if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
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
//
// Every write is confined to an os.Root opened on serverRoot, so a symlinked parent directory
// cannot redirect a placement outside the instance.
func Apply(changes []Change, serverRoot string) error {
	root, err := openServerRoot(serverRoot, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	for _, c := range changes {
		if c.Action == ActionSkip {
			continue
		}
		rel, err := checkDest(c.Dest)
		if err != nil {
			return err
		}
		in, err := os.Open(c.Source)
		if err != nil {
			return fmt.Errorf("open %s: %w", c.Source, err)
		}
		err = publishInto(root, rel, in)
		_ = in.Close()
		if err != nil {
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
// with nothing that knows about it. Every write or removal is confined to an os.Root opened on
// serverRoot, for the same reason Apply's is.
func Rollback(manifest []ManifestEntry, serverRoot, backupDir string) error {
	root, err := openServerRoot(serverRoot, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var errs []error
	touched := map[string]bool{}
	for _, e := range manifest {
		rel, err := checkDest(e.Path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		touched[filepath.Dir(rel)] = true
		saved := filepath.Join(backupDir, rel)

		switch _, err := os.Lstat(saved); {
		case err == nil:
			if err := restoreOne(root, rel, saved); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", e.Path, err))
			}
			continue
		case !errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("stat backup of %s: %w", e.Path, err))
			continue
		}
		if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
		}
	}
	return errors.Join(append(errs, removeStaleTemps(root, touched))...)
}

func restoreOne(root *os.Root, rel, saved string) error {
	f, err := os.Open(saved) //nolint:gosec // saved is under the job's own backupDir, joined
	// from a checkDest-validated relative path
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	return publishInto(root, rel, f)
}

// tempPrefix is what an in-flight write is called before it is renamed into place.
const tempPrefix = ".valmin-"

// removeStaleTemps deletes the half-written files a killed Apply left behind. No manifest names
// them: publishInto renames a temp file into place, so a process dying in between leaves
// `.valmin-XXXX` beside the file it was about to become.
//
// Only the directories this rollback touched, only regular files, only that prefix. The instance
// lock admits one mod job at a time and the server is stopped (B11), so such a file is this
// job's.
func removeStaleTemps(root *os.Root, dirs map[string]bool) error {
	var errs []error
	for dir := range dirs {
		entries, err := fs.ReadDir(root.FS(), dir)
		if errors.Is(err, fs.ErrNotExist) {
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
			rel := filepath.Join(dir, entry.Name())
			if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove the interrupted write %s: %w", rel, err))
			}
		}
	}
	return errors.Join(errs...)
}

// publishInto atomically writes in's bytes to rel inside root: a temp file beside the
// destination, fsynced and chmod'd to fsutil's mode, then renamed over it (ADR-075), so a
// killed write is never visible under the real name. Every step is a Root method, so a
// symlinked parent cannot redirect it outside serverRoot the way a lexical join would.
func publishInto(root *os.Root, rel string, in io.Reader) error {
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := mkdirAllSetgid(root, dir); err != nil {
			return fmt.Errorf("create the parent of %s: %w", rel, err)
		}
	}

	tmp, tmpRel, err := createTempIn(root, dir)
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", rel, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = root.Remove(tmpRel)
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy into %s: %w", tmpRel, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmpRel, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpRel, err)
	}
	if err := root.Chmod(tmpRel, fsutil.FileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpRel, err)
	}
	if err := root.Rename(tmpRel, rel); err != nil {
		return fmt.Errorf("publish %s: %w", rel, err)
	}
	return nil
}

// mkdirAllSetgid creates dir and any missing parent inside root, setgid included. Root.MkdirAll
// accepts only permission bits, so the setgid bit fsutil.DirMode carries is applied as a second
// step per path component, the way fsutil.MkdirAllExact applies it outside a Root.
func mkdirAllSetgid(root *os.Root, dir string) error {
	if err := root.MkdirAll(dir, fsutil.DirMode.Perm()); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	rel := "."
	for part := range strings.SplitSeq(filepath.ToSlash(dir), "/") {
		rel = filepath.Join(rel, part)
		if err := root.Chmod(rel, fsutil.DirMode); err != nil {
			return fmt.Errorf("chmod %s: %w", rel, err)
		}
	}
	return nil
}

// createTempIn opens a fresh, exclusively-created file named tempPrefix plus a random suffix
// inside dir, confined to root — os.Root has no CreateTemp of its own, so this is the same
// collision-retry os.CreateTemp does, over Root.OpenFile instead of a bare path.
func createTempIn(root *os.Root, dir string) (*os.File, string, error) {
	for range 100 {
		suffix := make([]byte, 8)
		if _, err := cryptorand.Read(suffix); err != nil {
			return nil, "", fmt.Errorf("generate a temp name: %w", err)
		}
		rel := filepath.Join(dir, tempPrefix+hex.EncodeToString(suffix))
		f, err := root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, rel, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", fmt.Errorf("open %s: %w", rel, err)
		}
	}
	return nil, "", errors.New("too many temp file collisions")
}

// publishFile publishes in at dest atomically, the same way publishInto does, for the two
// callers that write outside serverRoot: BackupPaths into the job's own backupDir, and
// CopyTree into a staging tree the panel just created. Neither destination is attacker-written
// at the time of the call, so a lexical join is what update.go's staging paths use too.
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
