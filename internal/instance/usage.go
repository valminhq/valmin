package instance

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Usage is one instance's on-disk footprint, broken down the way the decision about it is
// actually made. A single total answers "am I out of space" and nothing else; the split
// answers "what can I safely delete", which is the question that follows it — `server/` is a
// re-download, `backups/` is prunable, and `worlds/` is the one thing that is gone for good
// (`02 §5`).
type Usage struct {
	Server  uint64
	Worlds  uint64
	Logs    uint64
	Backups uint64
	Total   uint64
}

// DiskUsage measures an instance's footprint.
//
// `↯` **Allocated blocks, not apparent size.** This reports what `du` reports, because `du`
// is what an operator will check it against. Apparent size is the wrong number twice over:
// it ignores sparse files, and ADR-018 clones `server/` with `cp --reflink=auto`, so on btrfs
// or xfs two instances can apparently hold 1 GB each while the filesystem holds one copy.
// `↯` Blocks do not fix that second case — like `du`, each file still reports its own
// extents even where they are shared — so on a reflink filesystem this over-reports what
// freeing the instance would actually give back. Stated rather than hidden; `kv["data_fs_type"]`
// already records which filesystem is in play, and M0 measured ext4 as the common case,
// where the clone is a real copy and the number is exact.
//
// backupsDir is passed separately because backups do **not** live under the instance
// directory — `08 §5` deliberately does not mount them into any container, so they sit under
// `<data_root>/backups/<id>` instead (`02 §5`). An accounting that walked only dataDir would
// silently omit the one category that grows without bound.
func DiskUsage(dataDir, backupsDir string) (Usage, error) {
	// `↯` Each inode once, the way `du` counts. Nothing in the layout creates hard links
	// today, but a number that silently double-counts is one an operator could act on by
	// deleting a world to reclaim space that was never occupied.
	seen := map[uint64]bool{}

	var u Usage
	for _, part := range []struct {
		dir string
		to  *uint64
	}{
		{filepath.Join(dataDir, "server"), &u.Server},
		{filepath.Join(dataDir, "worlds"), &u.Worlds},
		{filepath.Join(dataDir, "logs"), &u.Logs},
		{backupsDir, &u.Backups},
	} {
		n, err := treeBytes(part.dir, seen)
		if err != nil {
			return Usage{}, err
		}
		*part.to = n
		u.Total += n
	}
	return u, nil
}

// treeBytes sums the allocated size of one tree. A directory that does not exist is zero,
// not an error: `server/` is published by rename at the end of a provision (`08 §3`) and
// `backups/<id>` does not exist until the first archive, so both are legitimately absent.
func treeBytes(root string, seen map[uint64]bool) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is normal here — a log rotates, a job finishes.
			// It is not a reason to fail the whole reading.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		n, err := entryBytes(path, d, seen)
		if err != nil {
			return err
		}
		total += n
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("measure %s: %w", root, err)
	}
	return total, nil
}

// entryBytes is one directory entry's contribution.
//
// Symlinks are counted as the link and never followed: WalkDir does not follow them, and
// following one inside `worlds/` — which a user owns, since it is a bind mount — would pull
// the whole host filesystem into the sum.
func entryBytes(path string, d fs.DirEntry, seen map[uint64]bool) (uint64, error) {
	info, err := d.Info()
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}
	if st.Nlink > 1 {
		if seen[uint64(st.Ino)] {
			return 0, nil
		}
		seen[uint64(st.Ino)] = true
	}
	// Blocks are always 512-byte units in stat(2), whatever the filesystem's block size.
	// The sign check is not ceremony: st_blocks is a signed field, and a total that wrapped
	// through uint64 would report an exabyte and send someone deleting worlds.
	if st.Blocks <= 0 {
		return 0, nil
	}
	return uint64(st.Blocks) * 512, nil
}
