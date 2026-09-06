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

// AppID is the dedicated server's Steam AppID, distinct from the game client's 892970.
const AppID = "896660"

// WantCloneUID is the uid every panel-owned file carries, including a freshly cloned server/.
// It is verified, never repaired by a chown that would mask a wrong-user clone (A3, A4).
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

// instanceDirMode is setgid so files written inside inherit the panel's group, plus
// group-write so an admin in that group can manage a world without sudo. Deliberately wider
// than gosec's generic default.
const instanceDirMode = 0o2775

// EnsureInstanceDirs creates worlds/ and logs/ ahead of container creation. server/ is left to
// Clone, which publishes it by rename so an interrupted provision leaves no empty directory.
func EnsureInstanceDirs(dataDir string) error {
	for _, sub := range []string{"worlds", "logs"} {
		dir := filepath.Join(dataDir, sub)
		if err := os.MkdirAll(dir, instanceDirMode); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}
	return nil
}

// BuildCacheInput is one SteamCMD run. HostCacheDir and CacheDir name the same directory as
// the host and the panel see it: the throwaway container's bind needs the former, every local
// operation the latter.
type BuildCacheInput struct {
	Runtime      runtime.Runtime
	Image        string
	HostCacheDir string
	CacheDir     string
	BuildID      string
	// Report, when set, is called before each retry with a human message, so a retrying run
	// does not read as a hang.
	Report func(attempt, of int, err error)
}

// steamCMDAttempts bounds SteamCMD's measured transient failure (Q31). It retries the step,
// not the job: the download touches no world and no container, and SteamCMD resumes it. A run
// that exhausts the attempts still fails loudly.
const (
	steamCMDAttempts = 3
)

// steamCMDRetryDelay is a var so a test can exercise the retry without the real backoff.
var steamCMDRetryDelay = 10 * time.Second

// buildCacheLocks serialises the callers writing one build-cache entry, keyed by its path.
// Entries are never removed: there is one per data root and build id.
var buildCacheLocks sync.Map

// lockBuildCacheEntry blocks until path has no other writer and returns its release.
func lockBuildCacheEntry(path string) func() {
	v, _ := buildCacheLocks.LoadOrStore(path, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnsureBuildCached publishes only under the installed manifest's build ID. The public
// branch can advance between lookup and download; the returned ID names the actual bytes.
func EnsureBuildCached(ctx context.Context, in *BuildCacheInput) (string, error) {
	if _, err := validSteamBuild(in.BuildID); err != nil {
		return "", err
	}
	defer lockBuildCacheEntry(in.CacheDir)()
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("wait for build cache: %w", err)
	}
	final := filepath.Join(in.CacheDir, in.BuildID)
	if _, err := os.Lstat(final); err == nil {
		return cachedBuildID(final, in.BuildID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat build cache: %w", err)
	}
	partLocal := filepath.Join(in.CacheDir, in.BuildID+".part")
	if err := os.MkdirAll(partLocal, instanceDirMode); err != nil {
		return "", fmt.Errorf("create build staging: %w", err)
	}
	if err := runSteamCMD(ctx, in, filepath.Join(in.HostCacheDir, in.BuildID+".part")); err != nil {
		return "", err
	}
	actual, err := ServerBuildID(partLocal)
	if err != nil {
		return "", fmt.Errorf("read downloaded build: %w", err)
	}
	if _, err := os.Stat(filepath.Join(partLocal, binaryMarker)); err != nil {
		return "", fmt.Errorf("verify downloaded binary: %w", err)
	}
	final = filepath.Join(in.CacheDir, actual)
	if _, err := os.Lstat(final); err == nil {
		if _, err := cachedBuildID(final, actual); err != nil {
			return "", err
		}
		if err := os.RemoveAll(partLocal); err != nil {
			return "", fmt.Errorf("discard duplicate build staging: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat downloaded build: %w", err)
	} else if err := os.Rename(partLocal, final); err != nil {
		return "", fmt.Errorf("publish build %s: %w", actual, err)
	}
	return actual, nil
}

func cachedBuildID(dir, want string) (string, error) {
	id, err := ServerBuildID(dir)
	if err != nil {
		return "", err
	}
	if id != want {
		return "", fmt.Errorf("cached build manifest does not match directory")
	}
	if _, err := os.Stat(filepath.Join(dir, binaryMarker)); err != nil {
		return "", fmt.Errorf("verify cached binary: %w", err)
	}
	return id, nil
}

// CachePublicBuild resolves the public branch before checking the immutable cache.
func CachePublicBuild(ctx context.Context, in *BuildCacheInput) (string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := QueryPublicBuild(queryCtx, in.Runtime, in.Image)
	if err != nil {
		return "", err
	}
	request := *in
	request.BuildID = id
	return EnsureBuildCached(ctx, &request)
}

// runSteamCMD runs the install, retrying a failed attempt up to steamCMDAttempts times. Only
// a run failure is retried; a done context ends it immediately.
func runSteamCMD(ctx context.Context, in *BuildCacheInput, partHost string) error {
	var last error
	for attempt := 1; attempt <= steamCMDAttempts; attempt++ {
		// The output is captured into the error: an exit code alone is unactionable.
		var out strings.Builder
		code, err := runtime.RunThrowaway(ctx, in.Runtime, &runtime.ThrowawaySpec{
			Image: in.Image,
			// Without this the container takes the image's own root, which drops every
			// capability here and cannot write the panel-owned output directory. It must be
			// 10000 specifically: this tree is cloned into server/, which A4 requires to be
			// 10000-owned with no repairing chown.
			User: containerUser,
			// SteamCMD writes `.steam` and its depot caches under $HOME, which uid 10000 does
			// not own in this image. Pointed at container-local scratch rather than the bind,
			// so that state does not end up cloned into every instance's server/.
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

// steamCMDErrorLines is how much of a failed run's output travels with the error.
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

// binaryMarker is the file whose presence means server/ holds a complete clone. It is what a
// repeated clone skips on, and what ownership is verified against (A4).
const binaryMarker = "valheim_server.x86_64"

// CloneWithProgress copies the cached build into an instance's server/ via a temp directory
// renamed on completion, so a half-copied server/ is never visible under its real name. report
// receives the percentage of srcDir's bytes copied so far, polled at pollInterval. A dstDir
// that already holds a complete clone is left untouched.
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

// cloneCommand builds `cp -a --reflink=auto <src>/. <dst>` as an argv, not a shell command
// line, so no shell reinterprets the paths (D8).
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

// reportCloneProgress polls the staging directory's size against the source's known total. A
// failed poll is skipped and retried on the next tick: this is an estimate, not a signal.
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

// VerifyClonedOwnership fails loudly if the clone did not run as wantUID, normally
// WantCloneUID. Never repaired by a chown, which would mask a server/ the game cannot write
// (A4). wantUID is a parameter so a test can assert both branches.
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

// Reflink-capable filesystem magic numbers (statfs(2)), spelled out rather than taken from
// golang.org/x/sys/unix, which does not name all of them consistently across versions.
const (
	fsMagicBtrfs = 0x9123683e
	fsMagicXFS   = 0x58465342
)

// ProbeFSType names the filesystem under path. btrfs and XFS make `cp --reflink=auto` a
// near-instant CoW clone; on ext4 it degrades silently to a full copy. An unidentified
// filesystem is reported as ext4, so the slow path is the assumption.
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

// CloneProgressBudget is the [start, end) percentage the clone phase occupies, sized from the
// probed filesystem type: a near-instant reflink clone gets a small slice, a presumed full copy
// most of the bar so its incremental reports have room to move.
func CloneProgressBudget(fsType string) (start, end int) {
	if reflinkCapable[fsType] {
		return 55, 60
	}
	return 20, 85
}
