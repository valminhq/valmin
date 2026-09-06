package instance

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	if err := os.MkdirAll(filepath.Join(cache, "21981590"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	writeSteamManifest(t, filepath.Join(cache, "21981590"))
	fake.CreateErr = errFailIfCalled

	_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
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
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c); c.Exit(0) }

	_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "21981590")); err != nil {
		t.Errorf("build cache was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "21981590.part")); !os.IsNotExist(err) {
		t.Error(".part directory must not survive a successful publish")
	}
}

// TestEnsureBuildCachedFailsOnNonZeroExitLeavesPartInPlace is Q22's resume guarantee: a
// failed run must leave the .part directory exactly where a retry (SteamCMD's own resume)
// can find it, never delete-and-restart.
func TestEnsureBuildCachedFailsOnNonZeroExitLeavesPartInPlace(t *testing.T) {
	cache := t.TempDir()
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.OnStart = func(c *runtime.FakeContainer) { c.Exit(1) }

	_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
	})
	if err == nil {
		t.Fatal("want an error for a non-zero steamcmd exit")
	}
	if _, err := os.Stat(filepath.Join(cache, "21981590.part")); err != nil {
		t.Errorf(".part directory must survive a failed run for the next resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "21981590")); !os.IsNotExist(err) {
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

// TestSteamCMDIsRetriedWithinTheStep is Q31, bounded.
//
// Measured: the identical command on an identical empty directory failed
// five times in a row with `Missing configuration` and then succeeded, with nothing changed
// between runs. Without this, that transient fault parks the instance in `error` with
// partial artefacts and the user's only recovery is to notice and re-run.
//
// It retries the step, not the job. `12 §9.4` keeps `provision` off the automatic
// retry list because a re-entered job could redo work that touched a world or a container;
// the build cache touches neither — it is a download into a shared directory that SteamCMD
// itself resumes (Q22).
func TestSteamCMDIsRetriedWithinTheStep(t *testing.T) {
	shortenSteamCMDBackoff(t)
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.ExitCodes = []int{1, 1, 0} // fails twice, then succeeds

	root := t.TempDir()
	var reported int
	_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd", BuildID: "21981590",
		HostCacheDir: root, CacheDir: root,
		Report: func(int, int, error) { reported++ },
	})
	if err != nil {
		t.Fatalf("EnsureBuildCached: %v", err)
	}
	if fake.Runs() != 3 {
		t.Errorf("steamcmd ran %d times, want 3", fake.Runs())
	}
	if reported != 2 {
		t.Errorf("the job was told about %d retries, want 2 — a silent retry reads as a hang", reported)
	}
	if _, err := os.Stat(filepath.Join(root, "21981590")); err != nil {
		t.Errorf("the cache entry was not published after a successful retry: %v", err)
	}
}

// TestSteamCMDGivesUpLoudly: three attempts is not a guarantee — five consecutive failures
// were measured — so exhausting them must still fail the job rather than publish a partial
// cache entry under its final name.
func TestSteamCMDGivesUpLoudly(t *testing.T) {
	shortenSteamCMDBackoff(t)
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.ExitCodes = []int{1, 1, 1, 1}

	root := t.TempDir()
	_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd", BuildID: "21981590",
		HostCacheDir: root, CacheDir: root,
	})
	if err == nil {
		t.Fatal("EnsureBuildCached succeeded after every attempt failed")
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("error does not say it retried: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "21981590")); statErr == nil {
		t.Error("a partial cache entry was published under its final name")
	}
}

// TestACancelledProvisionStopsRetrying: a job the operator cancelled must not sit through
// three attempts and two backoffs first (`12 §8`).
func TestACancelledProvisionStopsRetrying(t *testing.T) {
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.ExitCodes = []int{1, 1, 1}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	root := t.TempDir()
	started := time.Now()
	if _, err := EnsureBuildCached(ctx, &BuildCacheInput{
		Runtime: fake, Image: "steamcmd", BuildID: "21981590",
		HostCacheDir: root, CacheDir: root,
	}); err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if elapsed := time.Since(started); elapsed > steamCMDRetryDelay {
		t.Errorf("a cancelled run waited %v before giving up", elapsed)
	}
	// One attempt at most — and zero is also correct, because a cancelled context fails at
	// container creation before anything starts. What must not happen is three.
	if runs := fake.Runs(); runs > 1 {
		t.Errorf("steamcmd ran %d times after cancellation, want at most 1", runs)
	}
}

func shortenSteamCMDBackoff(t *testing.T) {
	t.Helper()
	previous := steamCMDRetryDelay
	steamCMDRetryDelay = time.Millisecond
	t.Cleanup(func() { steamCMDRetryDelay = previous })
}

// TestBuildCacheRunsSteamCMDAsTheOwningUID asserts the throwaway container runs SteamCMD as
// uid 10000, not the image's own root. Every container this runtime creates drops all
// capabilities (08 §5), so a root without CAP_DAC_OVERRIDE is a plain uid 0: against a
// cache directory the panel owns as `10000:10000 0775` it gets `r-x`, and SteamCMD's first
// write fails with EACCES. uid 10000 also needs a writable `$HOME`, which SteamCMD writes
// under too.
//
// The stub needs neither, and the provisioning integration test only runs at uid 10000
// (A4), so this is the only test exercising this path.
func TestBuildCacheRunsSteamCMDAsTheOwningUID(t *testing.T) {
	cache := t.TempDir()
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	var user string
	var env []string
	fake.OnStart = func(c *runtime.FakeContainer) {
		user, env = c.Spec.User, c.Spec.Env
		writeSteamInstall(t, c)
		c.Exit(0)
	}

	if _, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
		Runtime: fake, Image: "steamcmd/steamcmd:latest",
		HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
	}); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(env, "HOME=/tmp") {
		t.Errorf("steamcmd ran with env %v, want HOME set — the real image writes its own "+
			"state under $HOME, and the image's home does not belong to uid %s (08 §3.2)",
			env, containerUser)
	}
	if user != containerUser {
		t.Errorf("steamcmd ran as %q, want %q — the download writes a directory the panel "+
			"owns as that uid, and cap-drop ALL leaves container-root unable to (08 §2, §5)",
			user, containerUser)
	}
}

// TestEnsureBuildCachedRunsOneDownloadForConcurrentCallers is ADR-018's stated promise —
// "two instances provisioning against the same build converge on one download" — asserted
// rather than assumed. It was the intent and not the behaviour: the job engine's lock key is
// per instance, so two provisions run concurrently, and before the mutex both would hand the
// same `.part` directory to their own SteamCMD.
//
// The assertion is the *count of SteamCMD runs*, not the published directory. Both
// callers succeeding proves nothing — they would both "succeed" while corrupting each
// other's depot state, which surfaces later as `Missing configuration` or a `0x602` app
// state and is indistinguishable from Q31's real transient failure. Counting the runs is
// what distinguishes converging on one download from colliding on one directory.
func TestEnsureBuildCachedRunsOneDownloadForConcurrentCallers(t *testing.T) {
	cache := t.TempDir()

	var mu sync.Mutex
	runs := 0
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
	fake.OnStart = func(c *runtime.FakeContainer) {
		mu.Lock()
		runs++
		mu.Unlock()
		// Long enough that a second caller would overlap if nothing serialised them.
		time.Sleep(50 * time.Millisecond)
		writeSteamInstall(t, c)
		c.Exit(0)
	}

	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = EnsureBuildCached(t.Context(), &BuildCacheInput{
				Runtime: fake, Image: "steamcmd/steamcmd:latest",
				HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Errorf("steamcmd ran %d times for %d concurrent callers, want exactly 1", runs, callers)
	}
}

// TestEnsureBuildCachedDoesNotSerialiseSeparateCaches is the other side of the test above.
// The guarantee is one writer per cache entry — two callers writing the same directory
// corrupt each other's depot state. Callers writing *different* directories share nothing,
// and a lock held process-wide makes them queue anyway: one download's full duration is
// added to the wait of every unrelated one behind it. A process holds more than one data
// root in exactly one place — a test binary, where every test has its own — which is what
// made a 25-second failure path accumulate into a 90-second timeout (Q44).
func TestEnsureBuildCachedDoesNotSerialiseSeparateCaches(t *testing.T) {
	entered := make(chan struct{}, 2)
	proceed := make(chan struct{})

	download := func(cache string) <-chan error {
		done := make(chan error, 1)
		go func() {
			fake := runtime.NewFake()
			fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c) }
			fake.OnStart = func(c *runtime.FakeContainer) {
				entered <- struct{}{}
				<-proceed
				writeSteamInstall(t, c)
				c.Exit(0)
			}
			_, err := EnsureBuildCached(t.Context(), &BuildCacheInput{
				Runtime: fake, Image: "steamcmd/steamcmd:latest",
				HostCacheDir: cache, CacheDir: cache, BuildID: "21981590",
			})
			done <- err
		}()
		return done
	}

	first, second := download(t.TempDir()), download(t.TempDir())
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(proceed)
			t.Fatal("only one download started: two cache directories serialised on one lock")
		}
	}
	close(proceed)

	if err := <-first; err != nil {
		t.Errorf("first cache: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second cache: %v", err)
	}
}

func writeSteamManifest(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile("testdata/steam/appmanifest.acf")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "steamapps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "appmanifest_896660.acf"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, binaryMarker), []byte("game"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeSteamInstall(t *testing.T, c *runtime.FakeContainer) {
	t.Helper()
	writeSteamManifest(t, c.Spec.Binds[0].HostPath)
}

func TestBuildCacheUsesDownloadedBuildAndRetainsOldBuild(t *testing.T) {
	root := t.TempDir()
	writeSteamManifest(t, filepath.Join(root, "21981590"))
	fake := runtime.NewFake()
	fake.OnStart = func(c *runtime.FakeContainer) { writeSteamInstall(t, c); c.Exit(0) }
	id, err := EnsureBuildCached(
		t.Context(),
		&BuildCacheInput{Runtime: fake, Image: "steamcmd", CacheDir: root, HostCacheDir: root, BuildID: "21981589"},
	)
	if err != nil || id != "21981590" {
		t.Fatalf("build=%q: %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(root, "21981589")); !os.IsNotExist(err) {
		t.Fatal("download published under stale lookup ID")
	}
	data, err := os.ReadFile(filepath.Join(root, "21981590", binaryMarker))
	if err != nil || string(data) != "game" {
		t.Fatalf("old build overwritten: %q %v", data, err)
	}
}
