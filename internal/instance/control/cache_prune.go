package control

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// CacheGrace is how long an unused cache entry is kept after it was last written or handed
// out. It covers what a job outside the prune's locks has fetched but not yet recorded, such as
// the build a server created mid-prune is cloning.
const CacheGrace = time.Hour

// CacheKind names what a cache entry holds.
type CacheKind string

const (
	CacheModArchive      CacheKind = "mod_archive"
	CacheGameBuild       CacheKind = "game_build"
	CacheSetupBlob       CacheKind = "setup_blob"
	CachePartialDownload CacheKind = "partial_download"
)

// CacheServer names a server using a cache entry.
type CacheServer struct {
	ID   string
	Name string
}

// CacheEntry is one item of the panel's caches and who still uses it. An entry nothing uses
// may be pruned from PrunableAt on, which is zero while it is in use; Prunable reports that a
// prune now removes it, and ReclaimableBytes what that frees.
type CacheEntry struct {
	Kind             CacheKind
	Name             string
	Registry         string
	SizeBytes        int64
	ReclaimableBytes int64
	Servers          []CacheServer
	Setups           int
	PrunableAt       time.Time
	Prunable         bool

	usage fsutil.Usage
	// lastUsed is when the entry was last written or handed out.
	lastUsed time.Time
	// tree marks a directory, whose size is its whole tree and never shared by a hard link.
	tree bool
}

// InUse reports whether a server or a saved setup still names the entry.
func (e *CacheEntry) InUse() bool { return len(e.Servers) > 0 || e.Setups > 0 }

// CachePruner lists the mod archive, game build and setup blob caches against what servers and
// saved setups use, and removes what nothing uses.
type CachePruner struct {
	DB       *store.DB
	Engine   *jobs.Engine
	DataRoot string
	Builds   *instance.BuildCache
	// Caches holds one zip cache per registry. Those an installer downloads through must be the
	// installer's own, whose lock and in-flight downloads a prune respects.
	Caches map[source.Source]*cache.Cache
	Now    func() time.Time
}

// newCachePruner builds the pruner over the installer's zip caches, adding one for each
// registry the installer has none for, so a disabled registry's leftovers are listed too.
func newCachePruner(d *Deps, dataRoot string, builds *instance.BuildCache) *CachePruner {
	var installed map[source.Source]*cache.Cache
	if d.Installer != nil {
		installed = d.Installer.Caches
	}
	caches := make(map[source.Source]*cache.Cache, len(source.All()))
	for _, src := range source.All() {
		if c, ok := installed[src]; ok {
			caches[src] = c
		} else {
			caches[src] = cache.New(cache.RootFor(dataRoot, src))
		}
	}
	return &CachePruner{
		DB: d.DB, Engine: d.Engine, DataRoot: dataRoot, Builds: builds, Caches: caches, Now: time.Now,
	}
}

// Submit enqueues a prune. It holds every server's instance and chain locks, the setup artifact
// lock and the export lock, so nothing that installs, saves, restores, updates or copies cache
// files runs alongside it, and no sequence of jobs is cut between two steps.
func (p *CachePruner) Submit(ctx context.Context, spec *jobs.Spec) (*store.Job, error) {
	instances, err := p.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	spec.Kind = jobs.KindCachePrune
	spec.LockKey = jobs.GlobalLockKey(jobs.KindCachePrune)
	spec.LockKeys = []string{SetupArtifactLock, jobs.GlobalLockKey(jobs.KindExport)}
	for i := range instances {
		spec.LockKeys = append(spec.LockKeys,
			jobs.InstanceLockKey(instances[i].ID), jobs.ChainLockKey(instances[i].ID))
	}
	if spec.Payload == nil {
		spec.Payload = struct{}{}
	}
	j, err := p.Engine.Submit(ctx, spec, p.Run)
	if err != nil {
		return nil, fmt.Errorf("submit cache prune: %w", err)
	}
	return j, nil
}

// cacheUse is who uses what, read once per inventory or prune.
type cacheUse struct {
	archives map[string]*store.ArchiveUse
	builds   map[string][]string
	blobs    map[string]int
	names    map[string]string
}

// readUse reads what every server and saved setup uses. A server's build is the one its files
// carry, the one its row records, and the one an interrupted update was installing.
func (p *CachePruner) readUse(ctx context.Context) (*cacheUse, error) {
	instances, err := p.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	archives, err := p.DB.ModArchiveUse(ctx)
	if err != nil {
		return nil, err
	}
	blobs, err := p.DB.SetupArtifactUse(ctx)
	if err != nil {
		return nil, err
	}
	u := &cacheUse{
		archives: make(map[string]*store.ArchiveUse, len(archives)), blobs: blobs,
		builds: map[string][]string{}, names: map[string]string{},
	}
	for a, use := range archives {
		u.archives[archiveKey(a.Source, a.FullName+"-"+a.Version)] = use
	}
	installed := instance.InstalledBuilds(instances)
	for i := range instances {
		inst := &instances[i]
		u.names[inst.ID] = inst.Name
		ids := []string{installed[inst.ID], instance.StagedBuildID(inst.DataDir)}
		if inst.GameBuildID != nil {
			ids = append(ids, *inst.GameBuildID)
		}
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			if id != "" {
				u.builds[id] = append(u.builds[id], inst.ID)
			}
		}
	}
	return u, nil
}

// servers names the servers behind ids.
func (u *cacheUse) servers(ids []string) []CacheServer {
	out := make([]CacheServer, 0, len(ids))
	for _, id := range ids {
		name, ok := u.names[id]
		if !ok {
			name = id
		}
		out = append(out, CacheServer{ID: id, Name: name})
	}
	return out
}

// archiveKey keys a zip by registry and ident, the cache filename <full_name>-<version>.
func archiveKey(src, ident string) string { return src + "/" + ident }

// archive is the use of src's ident.
func (u *cacheUse) archive(src source.Source, ident string) (servers []string, setups int) {
	use := u.archives[archiveKey(src.String(), ident)]
	if use == nil {
		return nil, 0
	}
	return use.InstanceIDs, use.Setups
}

// Inventory lists every cache entry with its users and what pruning it now would free.
func (p *CachePruner) Inventory(ctx context.Context) ([]CacheEntry, error) {
	use, err := p.readUse(ctx)
	if err != nil {
		return nil, err
	}
	var entries []CacheEntry
	for _, src := range source.All() {
		zips, err := p.Caches[src].Entries()
		if err != nil {
			return nil, err
		}
		for i := range zips {
			entries = append(entries, use.zipEntry(src, &zips[i]))
		}
	}
	builds, err := p.Builds.Entries(instance.CacheDir(p.DataRoot))
	if err != nil {
		return nil, err
	}
	for i := range builds {
		entries = append(entries, use.buildEntry(&builds[i]))
	}
	blobs, err := setupblob.New(p.DataRoot).Entries()
	if err != nil {
		return nil, err
	}
	for i := range blobs {
		entries = append(entries, use.blobEntry(&blobs[i]))
	}
	markReclaimable(entries, p.Now().Add(-CacheGrace))
	return entries, nil
}

// zipEntry describes one file of src's zip cache. A zip is last used when it was downloaded or
// a cache hit touched it, both of which set its mtime.
func (u *cacheUse) zipEntry(src source.Source, z *cache.Entry) CacheEntry {
	e := CacheEntry{
		Kind:      CacheModArchive,
		Name:      z.Ident,
		Registry:  src.String(),
		SizeBytes: z.SizeBytes,
		usage:     z.Usage,
		lastUsed:  z.Modified,
	}
	if z.Partial {
		e.Kind = CachePartialDownload
	} else {
		ids, setups := u.archive(src, z.Ident)
		e.Servers, e.Setups = u.servers(ids), setups
	}
	return e
}

// buildEntry describes one directory of the game build cache, last used when it was published
// or handed out, both of which set its mtime.
func (u *cacheUse) buildEntry(b *instance.BuildEntry) CacheEntry {
	e := CacheEntry{
		Kind: CacheGameBuild, Name: b.Name, SizeBytes: b.SizeBytes, usage: b.Usage,
		lastUsed: b.Modified, tree: true,
	}
	if b.Partial {
		e.Kind = CachePartialDownload
	} else {
		e.Servers = u.servers(u.builds[b.BuildID])
	}
	return e
}

// blobEntry describes one file of the setup blob store. A setup file is judged by its ctime: the
// hard link that adds a cached zip to the store keeps the zip's mtime.
func (u *cacheUse) blobEntry(b *setupblob.Entry) CacheEntry {
	e := CacheEntry{
		Kind: CacheSetupBlob, Name: b.Name, SizeBytes: b.SizeBytes, usage: b.Usage, lastUsed: b.Changed,
	}
	if b.Partial {
		e.Kind = CachePartialDownload
	} else {
		e.Setups = u.blobs[b.Name]
	}
	return e
}

// markReclaimable sets when each unused entry becomes prunable and what pruning it now would
// free. A file's inode counts once, and only when every name linking it is prunable now, so a
// zip a setup blob also links frees nothing until both go.
func markReclaimable(entries []CacheEntry, cutoff time.Time) {
	type inodeKey struct{ device, inode uint64 }
	type inode struct {
		names, prunable, links uint64
		credited               bool
	}
	inodes := map[inodeKey]*inode{}
	key := func(e *CacheEntry) inodeKey { return inodeKey{e.usage.Device, e.usage.Inode} }
	for i := range entries {
		e := &entries[i]
		if !e.InUse() {
			e.PrunableAt = e.lastUsed.Add(CacheGrace)
		}
		e.Prunable = !e.InUse() && !e.lastUsed.After(cutoff)
		if e.tree {
			continue
		}
		n := inodes[key(e)]
		if n == nil {
			n = &inode{links: e.usage.Links}
			inodes[key(e)] = n
		}
		n.names++
		if e.Prunable {
			n.prunable++
		}
	}
	for i := range entries {
		e := &entries[i]
		if !e.Prunable {
			continue
		}
		if e.tree {
			e.ReclaimableBytes = e.SizeBytes
			continue
		}
		// The inode's size is credited to its first name only.
		if n := inodes[key(e)]; !n.credited && n.prunable == n.names && n.names >= n.links {
			e.ReclaimableBytes = e.SizeBytes
			n.credited = true
		}
	}
}

// Run removes every unused entry past its grace period: orphan setup blobs first, so a zip they
// linked becomes the last name of its bytes, then zips, then game builds and their leftovers.
// Builds are skipped while a download holds the build cache, rather than holding every server's
// lock until it ends.
func (p *CachePruner) Run(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	use, err := p.readUse(ctx)
	if err != nil {
		return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
	}
	cutoff := p.Now().Add(-CacheGrace)
	prunable := func(inUse bool, lastUsed time.Time) bool { return !inUse && !lastUsed.After(cutoff) }
	var tally pruneTally

	referenced := make(map[string]bool, len(use.blobs))
	for digest := range use.blobs {
		referenced[digest] = true
	}
	blobs, err := setupblob.New(p.DataRoot).GC(referenced, cutoff)
	for _, b := range blobs {
		tally.add(CacheSetupBlob, b.Partial, b.FreedBytes())
	}
	if err != nil {
		return tally.failed(jh, err)
	}
	jh.Progress(ctx, 20, tally.String())

	for _, src := range source.All() {
		zips, err := p.Caches[src].Prune(func(z cache.Entry) bool {
			ids, setups := use.archive(src, z.Ident)
			return prunable(!z.Partial && (len(ids) > 0 || setups > 0), z.Modified)
		})
		for _, z := range zips {
			tally.add(CacheModArchive, z.Partial, z.FreedBytes())
		}
		if err != nil {
			return tally.failed(jh, err)
		}
	}
	jh.Progress(ctx, 50, tally.String())

	builds, err := p.Builds.Prune(ctx, instance.CacheDir(p.DataRoot), func(b instance.BuildEntry) bool {
		return prunable(!b.Partial && len(use.builds[b.BuildID]) > 0, b.Modified)
	})
	if errors.Is(err, instance.ErrBuildCacheBusy) {
		jh.Log("game builds skipped: a download is in progress")
		err = nil
	}
	for _, b := range builds {
		tally.add(CacheGameBuild, b.Partial, b.SizeBytes)
	}
	if err != nil {
		return tally.failed(jh, err)
	}
	jh.Progress(ctx, 100, tally.String())
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// pruneTally counts what a prune removed and the space it freed.
type pruneTally struct {
	freedBytes int64
	counts     map[CacheKind]int
}

// add records one removed entry; a partial one counts as a partial download whatever held it.
func (t *pruneTally) add(kind CacheKind, partial bool, freedBytes int64) {
	if t.counts == nil {
		t.counts = map[CacheKind]int{}
	}
	if partial {
		kind = CachePartialDownload
	}
	t.counts[kind]++
	t.freedBytes += freedBytes
}

// String is the progress line: the space freed and what was removed.
func (t *pruneTally) String() string {
	return fmt.Sprintf("freed %s: %d mod archives, %d game builds, %d setup files, %d partial downloads",
		formatBytes(t.freedBytes), t.counts[CacheModArchive], t.counts[CacheGameBuild],
		t.counts[CacheSetupBlob], t.counts[CachePartialDownload])
}

// failed ends the prune on err, logging what it had already removed.
func (t *pruneTally) failed(jh *jobs.Handle, err error) jobs.Outcome {
	jh.Log(t.String())
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
}

// formatBytes renders a size for a message, in binary units with one decimal.
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := [...]string{"KiB", "MiB", "GiB", "TiB"}
	m := min((bits.Len64(uint64(n))-1)/10, len(units))
	return fmt.Sprintf("%.1f %s", float64(n)/float64(uint64(1)<<(m*10)), units[m-1])
}
