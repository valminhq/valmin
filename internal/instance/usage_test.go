package instance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
)

// write lays down a file of n bytes.
func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiskUsageSplitsTheCategoriesThatMatter. The split is the point: a single total says
// "you are out of space" and stops, where the breakdown says which of the three things an
// operator may safely delete — and `worlds/` is the one that is gone for good (02 §5).
func TestDiskUsageSplitsTheCategoriesThatMatter(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "instances", "i1")
	backups := filepath.Join(dir, "backups", "i1")

	write(t, filepath.Join(dataDir, "server", "valheim_server.x86_64"), 40*1024)
	write(t, filepath.Join(dataDir, "server", "lib", "steam.so"), 8*1024)
	write(t, filepath.Join(dataDir, "worlds", "worlds_local", "Midgard.db"), 16*1024)
	write(t, filepath.Join(dataDir, "logs", "console.log"), 4*1024)
	write(t, filepath.Join(backups, "2026-08-31.tar.gz"), 12*1024)

	u, err := instance.DiskUsage(dataDir, backups)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		got  uint64
		min  uint64
	}{
		{"server", u.Server, 48 * 1024},
		{"worlds", u.Worlds, 16 * 1024},
		{"logs", u.Logs, 4 * 1024},
		{"backups", u.Backups, 12 * 1024},
	} {
		if tc.got < tc.min {
			t.Errorf("%s = %d bytes, want at least %d", tc.name, tc.got, tc.min)
		}
	}
	if u.Total != u.Server+u.Worlds+u.Logs+u.Backups {
		t.Errorf("total %d is not the sum of its parts", u.Total)
	}
}

// `↯` Backups do not live under the instance directory — 08 §5 keeps them out of every
// container's binds, so they sit under <data_root>/backups/<id> (02 §5). An accounting that
// walked only dataDir would silently omit the one category that grows without bound.
func TestDiskUsageCountsBackupsFromOutsideTheInstanceDirectory(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "instances", "i1")
	backups := filepath.Join(dir, "backups", "i1")
	write(t, filepath.Join(dataDir, "worlds", "Midgard.db"), 4*1024)
	write(t, filepath.Join(backups, "old.tar.gz"), 64*1024)

	u, err := instance.DiskUsage(dataDir, backups)
	if err != nil {
		t.Fatal(err)
	}
	if u.Backups < 64*1024 {
		t.Fatalf("backups = %d, want at least 64 KiB — the separate root was not walked", u.Backups)
	}
	if u.Total <= u.Worlds {
		t.Error("total omits backups entirely")
	}
}

// A missing directory is zero, not an error: server/ is published by rename at the end of a
// provision (08 §3), and backups/<id> does not exist until the first archive.
func TestDiskUsageTreatsAbsentDirectoriesAsZero(t *testing.T) {
	dir := t.TempDir()
	u, err := instance.DiskUsage(filepath.Join(dir, "never-provisioned"), filepath.Join(dir, "no-backups"))
	if err != nil {
		t.Fatalf("an unprovisioned instance must measure, not fail: %v", err)
	}
	if u.Total != 0 {
		t.Errorf("total = %d, want 0", u.Total)
	}
}

// `↯` Each inode once, the way du counts. A number that silently double-counts is one an
// operator could act on by deleting a world to reclaim space that was never occupied.
func TestDiskUsageCountsAHardLinkedInodeOnce(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "i1")
	write(t, filepath.Join(dataDir, "worlds", "Midgard.db"), 64*1024)
	if err := os.Link(
		filepath.Join(dataDir, "worlds", "Midgard.db"),
		filepath.Join(dataDir, "worlds", "Midgard.db.link"),
	); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	u, err := instance.DiskUsage(dataDir, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Worlds >= 128*1024 {
		t.Errorf("worlds = %d bytes: the same inode was counted twice", u.Worlds)
	}
}

// A symlink is counted as the link and never followed. Following one inside worlds/ — which
// a user can create, since worlds/ is a bind mount they own — would pull the whole host
// filesystem into the sum.
//
// `↯` This one is weaker than the others and says so: `filepath.WalkDir` does not follow
// symlinks, so the property is the stdlib's rather than this code's, and the test cannot be
// broken by an edit to `treeBytes` alone. It earns its place by failing if anyone swaps in a
// following walker, which is the realistic way this would regress.
func TestDiskUsageDoesNotFollowSymlinksOutOfTheTree(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "i1")
	outside := filepath.Join(dir, "outside")
	write(t, filepath.Join(outside, "huge.bin"), 512*1024)
	write(t, filepath.Join(dataDir, "worlds", "Midgard.db"), 4*1024)
	if err := os.Symlink(outside, filepath.Join(dataDir, "worlds", "escape")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	u, err := instance.DiskUsage(dataDir, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Worlds >= 512*1024 {
		t.Errorf("worlds = %d bytes: the symlink was followed out of the tree", u.Worlds)
	}
}

// TestDiskUsageAgreesWithDu is the one that makes the number trustworthy. `du` is what an
// operator will check this against, so parity with it is the specification, not an
// implementation detail — and it is why this reports allocated blocks rather than apparent
// size.
func TestDiskUsageAgreesWithDu(t *testing.T) {
	du, err := exec.LookPath("du")
	if err != nil {
		t.Skip("du not available")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "i1")
	for i, n := range []int{40960, 8192, 1, 300000} {
		write(t, filepath.Join(dataDir, "server", "f"+strconv.Itoa(i)), n)
	}
	write(t, filepath.Join(dataDir, "worlds", "Midgard.db"), 65536)
	write(t, filepath.Join(dataDir, "logs", "console.log"), 1024)

	out, err := exec.Command(du, "-sB1", dataDir).Output()
	if err != nil {
		t.Fatalf("du: %v", err)
	}
	want, err := strconv.ParseUint(strings.Fields(string(out))[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	u, err := instance.DiskUsage(dataDir, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	// du includes the directory inodes themselves; this walks the three named subtrees, so
	// the instance root's own entry is the permitted difference.
	if diff := int64(want) - int64(u.Total); diff < 0 || diff > 8192 {
		t.Errorf("DiskUsage = %d, du = %d (difference %d bytes)", u.Total, want, diff)
	}
}
