package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/version"
)

// Dir is where exports are written: under the data root, and never itself exported.
func Dir(root string) string { return filepath.Join(root, "exports") }

// filePrefix starts the name of every export file, and workPrefix the staging directory of an
// export being written.
const (
	filePrefix = "valmin-export-"
	workPrefix = ".work-"
)

// Latest returns the newest finished export in Dir, or "" when there is none.
func Latest(root string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(Dir(root), filePrefix+"*.tar.gz"))
	if err != nil {
		return "", fmt.Errorf("list exports: %w", err)
	}
	if len(paths) == 0 {
		return "", nil
	}
	// The stamp in the name sorts by time.
	return slices.MaxFunc(paths, func(a, b string) int { return strings.Compare(stamp(a), stamp(b)) }), nil
}

// ModeOf reads the mode out of an export file's name.
func ModeOf(file string) Mode {
	mode, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(file), filePrefix), "-")
	return Mode(mode)
}

// stamp is the time part of an export file's name.
func stamp(p string) string { return p[strings.LastIndex(p, "-")+1:] }

// Exporter writes installation exports.
type Exporter struct {
	DB   *store.DB
	Root string
	// KeyFile is the panel's master key file. It is exported only while it holds the key Keeper
	// uses, which it does not when the key comes from the environment.
	KeyFile      string
	Keeper       *crypto.Keeper
	RcloneConfig string
	// FreeSpace reports the data root's free bytes; an export stops before they fall below
	// FreeSpaceFloor.
	FreeSpace      func() (uint64, error)
	FreeSpaceFloor uint64
}

// Job returns the runner of one export in mode. The caller holds the lock of every server but
// those in unlocked, so only those can change while they are copied; they are copied live.
func (e *Exporter) Job(mode Mode, unlocked []string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		dest, err := e.run(ctx, jh, mode, unlocked)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errCancelled) {
				return jobs.Outcome{Status: jobs.StatusCancelled}
			}
			return jobs.Failed(errcode.Internal, err)
		}
		size := ""
		if fi, err := os.Stat(dest); err == nil {
			size = fmt.Sprintf(" (%d MiB)", fi.Size()>>20)
		}
		jh.Progress(ctx, 100, "export ready: "+filepath.Base(dest)+size)
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

func (e *Exporter) run(ctx context.Context, jh *jobs.Handle, mode Mode, unlocked []string) (string, error) {
	dir := Dir(e.Root)
	if err := fsutil.MkdirAllExact(dir); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	work, err := os.MkdirTemp(dir, workPrefix)
	if err != nil {
		return "", fmt.Errorf("create export staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	jh.Progress(ctx, 5, "copying the database")
	db := filepath.Join(work, DatabaseName)
	if err := e.DB.CopyTo(ctx, db); err != nil {
		return "", err //nolint:wrapcheck // CopyTo names the copy
	}
	plan, err := e.plan(ctx, mode, db, unlocked)
	if err != nil {
		return "", err
	}
	for _, name := range plan.Manifest.Live {
		jh.Log("warning: " + name + " is running, so its files are copied as they are and may not be consistent")
	}
	if !e.Keeper.UsesKeyFile(plan.KeyFile) {
		plan.KeyFile = ""
		jh.Log("warning: " + e.KeyFile + " does not hold the master key this panel uses, so the export " +
			"leaves the key out; carry it to the new host yourself")
	}

	jh.Progress(ctx, 15, "writing the "+string(mode)+" export")
	name := filePrefix + string(mode) + "-" + plan.Manifest.CreatedAt.Format("20060102T150405Z") + ".tar.gz"
	dest := filepath.Join(dir, name)
	if err := writeAtomic(dest, plan, func() error { return e.mayContinue(ctx, jh) }); err != nil {
		return "", err
	}
	if err := removeOlder(dir, dest); err != nil {
		jh.Log("warning: " + err.Error())
	}
	return dest, nil
}

// plan reads what the export holds from the database. The servers in unlocked are copied live.
func (e *Exporter) plan(ctx context.Context, mode Mode, db string, unlocked []string) (*Plan, error) {
	schema, err := store.SchemaVersion()
	if err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	instances, err := e.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	p := &Plan{
		Manifest: Manifest{
			Format: formatVersion, Mode: mode, Schema: schema,
			Panel: version.Current().Version, CreatedAt: time.Now().UTC(),
		},
		Root: e.Root, Database: db, KeyFile: e.KeyFile, RcloneConfig: e.RcloneConfig,
	}
	builds := instance.InstalledBuilds(instances)
	for id, build := range builds {
		if build == "" {
			delete(builds, id)
		}
	}
	p.Manifest.Builds = builds
	var zips []string
	for i := range instances {
		inst := &instances[i]
		if inst.DataDir != instance.DataDir(e.Root, inst.ID) {
			return nil, fmt.Errorf("server %s keeps its data at %s, outside %s", inst.Name, inst.DataDir, e.Root)
		}
		mods, err := e.DB.InstanceMods(ctx, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("read the mods of %s: %w", inst.Name, err)
		}
		managed, archives, err := managedFiles(mods)
		if err != nil {
			return nil, fmt.Errorf("read the mods of %s: %w", inst.Name, err)
		}
		live := slices.Contains(unlocked, inst.ID)
		if live {
			p.Manifest.Live = append(p.Manifest.Live, inst.Name)
		}
		p.Instances = append(p.Instances, Instance{ID: inst.ID, Live: live, Managed: managed})
		zips = append(zips, archives...)
	}
	if mode == Full {
		p.Extra = e.downloadable(instances, builds, zips)
	}
	return p, nil
}

// managedFiles returns the server-relative paths installed packages place, and the
// root-relative paths of their cached archives.
func managedFiles(mods []store.InstanceMod) (managed map[string]bool, archives []string, err error) {
	managed = map[string]bool{}
	for i := range mods {
		m := &mods[i]
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(m.FileManifest), &manifest); err != nil {
			return nil, nil, fmt.Errorf("decode the manifest of %s: %w", m.FullName, err)
		}
		inServer, _ := installer.Split(manifest)
		for _, entry := range inServer {
			managed[entry.Path] = true
		}
		archives = append(archives, path.Join("cache", m.Source.String(), m.FullName+"-"+m.Version+".zip"))
	}
	return managed, archives, nil
}

// downloadable lists the root-relative paths a full export adds: the game builds and mod
// archives in use, every server's local world backups, and the saved-setup blobs.
func (e *Exporter) downloadable(instances []store.Instance, builds map[string]string, zips []string) []string {
	var extra []string
	steam, _ := filepath.Rel(e.Root, instance.CacheDir(e.Root))
	for _, id := range builds {
		extra = append(extra, path.Join(filepath.ToSlash(steam), id))
	}
	slices.Sort(extra)
	extra = slices.Compact(extra)

	slices.Sort(zips)
	extra = append(extra, slices.Compact(zips)...)
	for i := range instances {
		rel, _ := filepath.Rel(e.Root, filepath.Join(instance.BackupsDir(e.Root), instances[i].ID))
		extra = append(extra, filepath.ToSlash(rel))
	}
	blobs, _ := filepath.Rel(e.Root, setupblob.Dir(e.Root))
	return append(extra, filepath.ToSlash(blobs))
}

// CancelPolicy lets an export be cancelled at any point: nothing is published until it ends.
func CancelPolicy(string) (cancellable bool, phase string) { return true, "" }

// errCancelled ends an export whose job was cancelled or whose panel is shutting down.
var errCancelled = errors.New("export cancelled")

// mayContinue reports why the export must stop: a cancellation, the panel shutting down, or
// the data root falling below its free-space floor.
func (e *Exporter) mayContinue(ctx context.Context, jh *jobs.Handle) error {
	if ctx.Err() != nil || jh.CancelRequested(ctx) {
		return errCancelled
	}
	if e.FreeSpace == nil {
		return nil
	}
	free, err := e.FreeSpace()
	if err != nil {
		return fmt.Errorf("read free space: %w", err)
	}
	if free < e.FreeSpaceFloor {
		return fmt.Errorf("the data root is down to %d MiB free, below its floor of %d MiB; "+
			"free some room and export again", free>>20, e.FreeSpaceFloor>>20)
	}
	return nil
}

// checkBytes is how much an export writes between two calls to its check.
const checkBytes = 64 << 20

// checkedWriter fails every write once check returns an error, asked every checkBytes.
type checkedWriter struct {
	w     io.Writer
	check func() error
	since int
}

func (c *checkedWriter) Write(p []byte) (int, error) {
	c.since += len(p)
	if c.since >= checkBytes {
		c.since = 0
		if err := c.check(); err != nil {
			return 0, err
		}
	}
	return c.w.Write(p) //nolint:wrapcheck // the file's own error
}

// writeAtomic writes p to dest through a `.part` file, published by rename once complete. It
// stops when check returns an error.
func writeAtomic(dest string, p *Plan, check func() error) error {
	part := dest + backup.PartSuffix
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // panel-built path
	if err != nil {
		return fmt.Errorf("create export: %w", err)
	}
	published := false
	defer func() {
		_ = f.Close()
		if !published {
			_ = os.Remove(part)
		}
	}()
	if err := Write(&checkedWriter{w: f, check: check}, p); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush export: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close export: %w", err)
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("publish export: %w", err)
	}
	published = true
	return fsutil.SyncDir(filepath.Dir(dest)) //nolint:wrapcheck // SyncDir names the directory
}

// removeOlder deletes everything in dir but keep: older exports, and what an interrupted
// export left behind.
func removeOlder(dir, keep string) error {
	return removeIn(dir, func(name string) bool { return name != filepath.Base(keep) })
}

// Sweep removes what an export the panel was killed in the middle of left in Dir(root): its
// staging directory and unfinished archive. Finished exports stay.
func Sweep(root string) error {
	err := removeIn(Dir(root), func(name string) bool {
		return strings.HasPrefix(name, workPrefix) || strings.HasSuffix(name, backup.PartSuffix)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// removeIn deletes every entry of dir whose name drop selects.
func removeIn(dir string, drop func(name string) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list exports: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if !drop(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(errs...)
}
