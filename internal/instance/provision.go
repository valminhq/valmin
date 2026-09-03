package instance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/valminhq/valmin/internal/runtime"
)

// AppID is the dedicated server's Steam AppID, distinct from 892970 — the game client's
// own id, which the launched process sets in its own environment.
const AppID = "896660"

// WantCloneUID is the uid every panel-owned file carries, including a freshly cloned
// server/ (A3, A4). It is verified rather than assumed, because a defensive chown would
// mask a clone that ran as the wrong user.
const WantCloneUID = 10000

// CacheDir is the build cache root for one filesystem root — either side of the host/panel
// path split, since the caller picks which one it needs.
func CacheDir(dataRoot string) string {
	return filepath.Join(dataRoot, "cache", "steam", AppID)
}

// ImportStagingRoot is where a streamed upload lands before it is validated: under
// data.root, on the same filesystem as worlds/, so the install is a rename rather than a
// second copy of a multi-hundred-megabyte world.
func ImportStagingRoot(dataRoot string) string {
	dir := filepath.Join(dataRoot, "staging")
	_ = os.MkdirAll(dir, instanceDirMode)
	return dir
}

// BackupsDir is where archives live. It is deliberately not mounted into any container, so
// a compromised game server cannot reach the backups of the world it is running.
func BackupsDir(dataRoot string) string { return filepath.Join(dataRoot, "backups") }

// instanceDirMode is setgid, so files written inside inherit the panel's group, plus
// group-write, so an admin added to that host group can manage a world without sudo — the
// reason bind mounts were chosen. Deliberately wider than gosec's generic default.
const instanceDirMode = 0o2775

// EnsureInstanceDirs creates worlds/ and logs/ ahead of container creation. server/ is
// deliberately not created here: Clone publishes it atomically by rename, so an interrupted
// provision never leaves a directory that looks real but is empty.
func EnsureInstanceDirs(dataDir string) error {
	for _, sub := range []string{"worlds", "logs"} {
		dir := filepath.Join(dataDir, sub)
		if err := os.MkdirAll(dir, instanceDirMode); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}
	return nil
}

// BuildCacheInput is one SteamCMD run. HostCacheDir and CacheDir name the same host
// directory as the host and the panel container see it respectively: the throwaway
// container's bind needs the former, everything the panel does locally needs the latter.
type BuildCacheInput struct {
	Runtime      runtime.Runtime
	Image        string
	HostCacheDir string
	CacheDir     string
	BuildID      string
	// Report, when set, is called before each retry with a human message. The provision job
	// passes its progress reporter so a run that is retrying does not read as a hang.
	Report func(attempt, of int, err error)
}

// steamCMDAttempts bounds SteamCMD's transient failure (Q31): the identical command on an
// identical empty directory has been measured failing five times in a row with `Missing
// configuration` and then succeeding, with nothing changed between runs.
//
// This retries the step, not the job. A provision job is kept off the automatic-retry list
// because a re-entered job could re-run work that touched a world or a container, but the
// build cache touches neither: it is a download into a shared directory, keyed by build id,
// that SteamCMD itself resumes. Three attempts is therefore not a guarantee against five
// consecutive failures; a run that exhausts them still fails loudly.
const (
	steamCMDAttempts = 3
)

// steamCMDRetryDelay is a var so a test can prove the retry without waiting out the real
// backoff. Nothing else reassigns it.
var steamCMDRetryDelay = 10 * time.Second

// buildCacheMu serialises build-cache downloads. See the note inside EnsureBuildCached.
var buildCacheMu sync.Mutex

// EnsureBuildCached runs SteamCMD into <cache>/<buildID>/, or does nothing if that
// directory already exists: two instances provisioning against the same build converge on
// one download, and a resumed provision past this checkpoint re-runs into a no-op.
//
// It downloads into `<buildID>.part` and renames into place only on success, so a
// half-written cache entry is never visible under its final name. SteamCMD tolerates being
// killed mid-download and resumes, so a crash here needs no delete-and-restart path: the
// same `.part` directory is handed to SteamCMD again.
func EnsureBuildCached(ctx context.Context, in *BuildCacheInput) error {
	final := filepath.Join(in.CacheDir, in.BuildID)
	if _, err := os.Stat(final); err == nil {
		return nil
	}

	// Serialised because the job engine's lock key is per instance, so two provisions run
	// concurrently by design: both would find `final` missing, both MkdirAll the same
	// `.part`, and both hand that one directory to SteamCMD. Two SteamCMD processes sharing
	// an install directory corrupt each other's depot state and fail with `Missing
	// configuration` or a `0x602` app state — indistinguishable from Q31's genuine transient
	// failure, and so liable to be blamed on it.
	//
	// A process-level mutex is enough: one daemon owns the data root at a time, enforced by
	// the lease in .valmind.lock. Not keyed by build id — there is one build id today, and a
	// second caller waiting out a download it was going to skip anyway costs nothing.
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	// The wait may have been the download this call would otherwise have started.
	if _, err := os.Stat(final); err == nil {
		return nil
	}

	partLocal := filepath.Join(in.CacheDir, in.BuildID+".part")
	if err := os.MkdirAll(partLocal, instanceDirMode); err != nil {
		return fmt.Errorf("create build cache staging dir: %w", err)
	}
	partHost := filepath.Join(in.HostCacheDir, in.BuildID+".part")

	if err := runSteamCMD(ctx, in, partHost); err != nil {
		return err
	}

	if err := os.Rename(partLocal, final); err != nil {
		return fmt.Errorf("publish build cache %s: %w", in.BuildID, err)
	}
	return nil
}

// runSteamCMD runs the install, retrying a failed attempt up to steamCMDAttempts times.
//
// Only a run failure is retried: a context that is done ends it immediately, because a
// cancelled provision retrying three times is a job ignoring the operator.
func runSteamCMD(ctx context.Context, in *BuildCacheInput, partHost string) error {
	var last error
	for attempt := 1; attempt <= steamCMDAttempts; attempt++ {
		// The output is captured into the error. An exit code alone is unactionable: Q31 was
		// diagnosed by reading what SteamCMD actually printed, and a job that fails with
		// "exited 1" gives the operator nothing to read.
		var out strings.Builder
		code, err := runtime.RunThrowaway(ctx, in.Runtime, &runtime.ThrowawaySpec{
			Image: in.Image,
			// Without this the download cannot write its own output directory. The container
			// would take the image's own user — root, for `steamcmd/steamcmd` — and every
			// container this runtime creates drops all capabilities, so that root has no
			// CAP_DAC_OVERRIDE and is a plain uid 0 against a directory the panel owns. `0775`
			// owned by 10000 gives uid 0 `r-x`, and `mkdir /out/linux64` then fails with
			// EACCES on SteamCMD's first write.
			//
			// 10000 rather than the panel's own uid because this tree is cloned into `server/`,
			// and A4 requires that to be 10000-owned with no repairing chown. The cache is the
			// source of the clone, so it carries the same identity.
			User: containerUser,
			// SteamCMD writes its own state — `.steam`, depot caches, a config — under $HOME,
			// and the image's HOME belongs to the image's user. Running as 10000 therefore
			// lands on a home directory this uid does not own, and the real image fails with a
			// bare `mkdir: Permission denied` before it logs in.
			//
			// `/tmp` inside the container, not the bind: Steam's state is scratch, and pointing
			// HOME at `/out` would sweep `.steam` and friends into the build cache, which is
			// then cloned into every instance's `server/`. The cost is that Steam's depot cache
			// does not survive a run; the download still resumes, because that lives in `/out`.
			Env: []string{"HOME=/tmp"},
			Cmd: []string{
				"+force_install_dir", "/out",
				"+login", "anonymous",
				"+app_update", AppID, "validate",
				"+quit",
			},
			Binds:  []runtime.Bind{{HostPath: partHost, ContainerPath: "/out"}},
			Stdout: &out,
			Stderr: &out,
		})
		switch {
		case err != nil:
			last = fmt.Errorf("run steamcmd for build %s: %w", in.BuildID, err)
		case code != 0:
			last = fmt.Errorf("steamcmd for build %s exited %d: %s",
				in.BuildID, code, lastLines(out.String(), steamCMDErrorLines))
		default:
			return nil
		}

		if ctx.Err() != nil {
			return last
		}
		if attempt == steamCMDAttempts {
			break
		}
		slog.WarnContext(ctx, "steamcmd failed, retrying",
			slog.String("build_id", in.BuildID),
			slog.Int("attempt", attempt), slog.Int("of", steamCMDAttempts),
			slog.Any("error", last))
		if in.Report != nil {
			in.Report(attempt, steamCMDAttempts, last)
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(steamCMDRetryDelay):
		}
	}
	return fmt.Errorf("after %d attempts: %w", steamCMDAttempts, last)
}

// steamCMDErrorLines is how much of a failed run's output travels with the error: enough
// to carry the message that explains it, not so much that a job row swallows a whole
// download log.
const steamCMDErrorLines = 5

// lastLines returns the final n non-empty lines of s, joined, for an error message.
func lastLines(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "; ")
}

// binaryMarker is the file whose presence means "this server/ is a real, complete clone" —
// used both to skip a clone that already succeeded and to verify who owns it (A4).
const binaryMarker = "valheim_server.x86_64"

// CloneWithProgress copies the cached build into an instance's server/ via a temp
// directory renamed on completion, so a half-copied server/ is never visible under its real
// name. report is called with the percentage of srcDir's bytes copied so far, polled at
// pollInterval, which is what keeps a full ~1 GB copy on a non-reflink filesystem from
// reading as a hang. A dstDir that already contains a complete clone is left untouched.
func CloneWithProgress(
	ctx context.Context,
	srcDir, dstDir string,
	pollInterval time.Duration,
	report func(pct int),
) error {
	if _, err := os.Stat(filepath.Join(dstDir, binaryMarker)); err == nil {
		report(100)
		return nil
	}
	total, err := dirSize(srcDir)
	if err != nil {
		return fmt.Errorf("measure %s: %w", srcDir, err)
	}

	tmp := dstDir + ".tmp"
	if err := resetCloneStaging(tmp); err != nil {
		return err
	}

	cmd := cloneCommand(ctx, srcDir, tmp)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start clone %s: %w", srcDir, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(max(pollInterval, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-done:
			return finishClone(waitErr, tmp, dstDir, report)
		case <-ticker.C:
			reportCloneProgress(tmp, total, report)
		}
	}
}

func resetCloneStaging(tmp string) error {
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear stale clone staging dir: %w", err)
	}
	if err := os.MkdirAll(tmp, instanceDirMode); err != nil {
		return fmt.Errorf("create clone staging dir: %w", err)
	}
	return nil
}

// cloneCommand builds `cp -a --reflink=auto <src>/. <dst>` as an argv rather than a shell
// command line: exec passes srcDir and tmp to cp as literal arguments, with no shell in the
// path to reinterpret them (D8).
func cloneCommand(ctx context.Context, srcDir, tmp string) *exec.Cmd {
	return exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", srcDir+"/.", tmp) //nolint:gosec // see comment above
}

func finishClone(waitErr error, tmp, dstDir string, report func(pct int)) error {
	if waitErr != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clone to %s: %w", dstDir, waitErr)
	}
	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("clear stale %s: %w", dstDir, err)
	}
	if err := os.Rename(tmp, dstDir); err != nil {
		return fmt.Errorf("publish clone to %s: %w", dstDir, err)
	}
	report(100)
	return nil
}

// reportCloneProgress polls the staging directory's size against the source's known total.
// A poll that fails (a rename mid-walk, e.g.) is silently skipped — this is a progress
// estimate, not a correctness signal, and the next tick tries again.
func reportCloneProgress(tmp string, total int64, report func(pct int)) {
	if total <= 0 {
		return
	}
	if copied, err := dirSize(tmp); err == nil {
		report(int(min(int64(99), copied*100/total)))
	}
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("walk %s: %w", root, err)
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", d.Name(), err)
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure directory size of %s: %w", root, err)
	}
	return total, nil
}

// VerifyClonedOwnership fails loudly if the clone did not run as wantUID (A4, normally
// WantCloneUID). The correct response is failing the job, never a defensive chown that
// would mask a clone which produced a server/ the game cannot write. wantUID is a parameter
// so a test can assert both branches without owning a file as uid 10000.
func VerifyClonedOwnership(dstDir string, wantUID int) error {
	fi, err := os.Stat(filepath.Join(dstDir, binaryMarker))
	if err != nil {
		return fmt.Errorf("verify clone ownership: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("verify clone ownership: no uid information for %s", dstDir)
	}
	if int(st.Uid) != wantUID {
		return fmt.Errorf(
			"clone of %s is owned by uid %d, want %d — the clone ran as the wrong user (A4)",
			dstDir, st.Uid, wantUID,
		)
	}
	return nil
}

// Reflink-capable filesystem magic numbers (statfs(2)), measured rather than named from
// golang.org/x/sys/unix so the check does not depend on that package shipping every magic
// constant under a matching name across versions.
const (
	fsMagicBtrfs = 0x9123683e
	fsMagicXFS   = 0x58465342
)

// ProbeFSType names the filesystem under path: btrfs and XFS can make `cp --reflink=auto`
// a near-instant CoW clone, while on ext4 — the common case — the same command degrades
// silently to a full ~1 GB copy. Anything this cannot identify is treated as ext4, because
// the safe assumption is the slow path rather than an assumed-fast one that looks hung.
func ProbeFSType(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown"
	}
	switch int64(st.Type) { //nolint:unconvert // Type's width differs by architecture
	case fsMagicBtrfs:
		return "btrfs"
	case fsMagicXFS:
		return "xfs"
	default:
		return "ext4"
	}
}

// reflinkCapable is ProbeFSType's answers that make the clone phase fast enough to budget
// a small slice of the overall progress bar.
var reflinkCapable = map[string]bool{"btrfs": true, "xfs": true}

// CloneProgressBudget is the [start, end) percentage the clone phase occupies, chosen from
// the probed filesystem type: reflink-capable filesystems clone near-instantly and get a
// small slice, while everything else is presumed to degrade to a full copy and gets the
// majority of the bar, so the incremental reports have room to move rather than sitting at
// one number.
func CloneProgressBudget(fsType string) (start, end int) {
	if reflinkCapable[fsType] {
		return 55, 60
	}
	return 20, 85
}
