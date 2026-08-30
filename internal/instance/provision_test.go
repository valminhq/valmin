package instance

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

func TestEnsureInstanceDirsCreatesWorldsAndLogsButNotServer(t *testing.T) {
	dataDir := t.TempDir()
	if err := EnsureInstanceDirs(dataDir); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"worlds", "logs"} {
		fi, err := os.Stat(filepath.Join(dataDir, sub))
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "server")); !os.IsNotExist(err) {
		t.Error("server/ must not exist until Clone publishes it — a half-provisioned " +
			"instance must not look like a real one")
	}
}

func TestEnsureBuildCachedSkipsWhenAlreadyPresent(t *testing.T) {
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "buildA"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := runtime.NewFake()
	fake.CreateErr = errFailIfCalled

	err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "buildA",
	})
	if err != nil {
		t.Fatalf("want no error (already cached, steamcmd never invoked), got %v", err)
	}
}

var errFailIfCalled = &fakeErr{"steamcmd must not run when the build is already cached"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func TestEnsureBuildCachedRunsSteamCMDAndPublishes(t *testing.T) {
	cache := t.TempDir()
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { c.Exit(0) }

	err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "buildB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "buildB")); err != nil {
		t.Errorf("build cache was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "buildB.part")); !os.IsNotExist(err) {
		t.Error(".part directory must not survive a successful publish")
	}
}

// TestEnsureBuildCachedFailsOnNonZeroExitLeavesPartInPlace is Q22's resume guarantee: a
// failed run must leave the .part directory exactly where a retry (SteamCMD's own resume)
// can find it, never delete-and-restart.
func TestEnsureBuildCachedFailsOnNonZeroExitLeavesPartInPlace(t *testing.T) {
	cache := t.TempDir()
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { c.Exit(1) }

	err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "buildC",
	})
	if err == nil {
		t.Fatal("want an error for a non-zero steamcmd exit")
	}
	if _, err := os.Stat(filepath.Join(cache, "buildC.part")); err != nil {
		t.Errorf(".part directory must survive a failed run for the next resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "buildC")); !os.IsNotExist(err) {
		t.Error("a failed run must never publish under the final name")
	}
}

func TestCloneWithProgressCopiesFilesAndReachesComplete(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, binaryMarker), make([]byte, 4096), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "steam_appid.txt"), []byte("896660"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "server")
	var reports []int
	err := CloneWithProgress(t.Context(), src, dst, time.Millisecond, func(pct int) {
		reports = append(reports, pct)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, binaryMarker)); err != nil {
		t.Errorf("clone did not publish the binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "steam_appid.txt")); err != nil {
		t.Errorf("clone did not carry the second file: %v", err)
	}
	if len(reports) == 0 || reports[len(reports)-1] != 100 {
		t.Errorf("reports = %v, want the last one to be 100", reports)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Error("staging directory must not survive a successful clone")
	}
}

func TestCloneWithProgressSkipsAnAlreadyCompleteClone(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, binaryMarker), []byte("already here"), 0o755); err != nil {
		t.Fatal(err)
	}

	calls := 0
	// A source that does not exist would fail a real copy — proving this path never tries.
	err := CloneWithProgress(t.Context(), "/no/such/source", dst, time.Millisecond, func(pct int) {
		calls++
		if pct != 100 {
			t.Errorf("report(%d), want 100 for an already-complete clone", pct)
		}
	})
	if err != nil {
		t.Fatalf("want no error for an already-cloned destination, got %v", err)
	}
	if calls != 1 {
		t.Errorf("report called %d times, want exactly 1", calls)
	}
}

func TestVerifyClonedOwnershipAcceptsTheCurrentUID(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, binaryMarker), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyClonedOwnership(dst, os.Getuid()); err != nil {
		t.Errorf("want no error for the file's real owner, got %v", err)
	}
}

func TestVerifyClonedOwnershipRejectsAnyOtherUID(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, binaryMarker), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyClonedOwnership(dst, os.Getuid()+1); err == nil {
		t.Error("want an error when the owning uid does not match — A4 forbids a silent chown instead")
	}
}

func TestProbeFSTypeUnknownPathIsUnknown(t *testing.T) {
	if got := ProbeFSType("/no/such/path/at/all"); got != "unknown" {
		t.Errorf("got %q, want unknown", got)
	}
}

func TestCloneProgressBudgetGivesReflinkFilesystemsASmallSlice(t *testing.T) {
	for _, fsType := range []string{"btrfs", "xfs"} {
		start, end := CloneProgressBudget(fsType)
		if end-start > 10 {
			t.Errorf("%s: budget %d-%d, want a small reflink-fast slice", fsType, start, end)
		}
	}
}

func TestCloneProgressBudgetGivesEverythingElseTheMajorityOfTheBar(t *testing.T) {
	for _, fsType := range []string{"ext4", "unknown", "nfs"} {
		start, end := CloneProgressBudget(fsType)
		if end-start < 50 {
			t.Errorf("%s: budget %d-%d, want the majority slice (full-copy assumption)", fsType, start, end)
		}
	}
}
