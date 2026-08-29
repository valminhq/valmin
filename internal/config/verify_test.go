package config

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
)

// catFake is a Fake that behaves like the check container: it resolves the entrypoint's
// path through the container's binds and writes the file it finds. Emulating the bind
// rather than the answer is what makes a wrong data.host_root fail here for the same
// reason it fails against a daemon.
func catFake() *runtime.Fake {
	f := runtime.NewFake()
	f.OnStart = func(c *runtime.FakeContainer) {
		if len(c.Spec.Entrypoint) != 2 || c.Spec.Entrypoint[0] != "/bin/cat" {
			c.Stderr("fake: unexpected entrypoint\n")
			c.Exit(127)
			return
		}
		host, ok := resolveBind(c.Spec.Binds, c.Spec.Entrypoint[1])
		if !ok {
			c.Stderr("cat: no such file or directory\n")
			c.Exit(1)
			return
		}
		body, err := os.ReadFile(host)
		if err != nil {
			c.Stderr("cat: " + err.Error() + "\n")
			c.Exit(1)
			return
		}
		c.Stdout(string(body))
		c.Exit(0)
	}
	return f
}

func resolveBind(binds []runtime.Bind, path string) (string, bool) {
	for _, b := range binds {
		rel, err := filepath.Rel(b.ContainerPath, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		return filepath.Join(b.HostPath, rel), true
	}
	return "", false
}

func hostCheckConfig(t *testing.T, hostRoot string) *Config {
	t.Helper()
	cfg := Defaults()
	cfg.Data.Root = t.TempDir()
	cfg.Data.HostRoot = hostRoot
	if hostRoot == "" {
		cfg.Data.HostRoot = cfg.Data.Root
	}
	return &cfg
}

func TestHostRootVerifiedWhenTheMountResolvesToTheSameDirectory(t *testing.T) {
	cfg := hostCheckConfig(t, "")

	if err := VerifyHostRoot(t.Context(), catFake(), cfg); err != nil {
		t.Fatalf("VerifyHostRoot: %v", err)
	}
}

// The failure 02 §5 calls the most common bug in this class of application: data.host_root
// left as the path inside the panel container. Every instance would start with an empty
// world directory and generate a brand new world, which looks like success.
func TestHostRootRefusedWhenTheMountIsADifferentDirectory(t *testing.T) {
	cfg := hostCheckConfig(t, t.TempDir())

	err := VerifyHostRoot(t.Context(), catFake(), cfg)
	if err == nil {
		t.Fatal("VerifyHostRoot accepted a data.host_root that mounts another directory")
	}
	for _, want := range []string{cfg.Data.Root, cfg.Data.HostRoot, "path on the host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so the operator cannot act on it:\n%v", want, err)
		}
	}
}

// Two panels sharing one data.host_root read a real file holding someone else's token.
// That is a different mistake from an empty mount and says so.
func TestHostRootRefusedWhenTheTokenBelongsToAnotherPanel(t *testing.T) {
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, hostCheckFile), []byte("another panel's token"), 0o644); err != nil {
		t.Fatalf("seed the other panel's token: %v", err)
	}
	cfg := hostCheckConfig(t, other)

	err := VerifyHostRoot(t.Context(), catFake(), cfg)
	if err == nil {
		t.Fatal("VerifyHostRoot accepted another panel's token")
	}
	if !strings.Contains(err.Error(), "different token") {
		t.Errorf("refusal does not distinguish a stale token from an absent file:\n%v", err)
	}
}

// Q27's constraint: a self-check whose image is missing must refuse, never no-op. A check
// that silently passes when it cannot run is worse than no check (C22).
func TestHostRootRefusedWhenTheImageIsMissing(t *testing.T) {
	cfg := hostCheckConfig(t, "")
	f := catFake()
	f.CreateErr = errors.New("No such image: valmin/valheim:dev")

	err := VerifyHostRoot(t.Context(), f, cfg)
	if err == nil {
		t.Fatal("VerifyHostRoot passed without running the check")
	}
	if !strings.Contains(err.Error(), cfg.Game.Image) {
		t.Errorf("refusal does not name the image it needed:\n%v", err)
	}
}

func TestHostRootCheckRunsWithoutNetworkAndReadOnly(t *testing.T) {
	cfg := hostCheckConfig(t, "")
	f := catFake()

	var got runtime.ContainerSpec
	inner := f.OnStart
	f.OnStart = func(c *runtime.FakeContainer) {
		got = c.Spec
		inner(c)
	}
	if err := VerifyHostRoot(t.Context(), f, cfg); err != nil {
		t.Fatalf("VerifyHostRoot: %v", err)
	}

	if !got.NetworkDisabled {
		t.Error("the check container has a network; it has no reason to reach anything (10 §1.2)")
	}
	if len(got.Binds) != 1 || !got.Binds[0].ReadOnly {
		t.Errorf("data.host_root must be mounted read-only, got %+v", got.Binds)
	}
	if got.Binds[0].HostPath != cfg.Data.HostRoot {
		t.Errorf("mounted %q, want data.host_root %q", got.Binds[0].HostPath, cfg.Data.HostRoot)
	}
	want := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	if got.User != want {
		t.Errorf("check container runs as %q, want the panel's own %q: reading back a file "+
			"the panel wrote must not depend on a second identity", got.User, want)
	}
}

func TestDataRootAcceptsAWritableDirectoryWithRoom(t *testing.T) {
	cfg := Defaults()
	cfg.Data.Root = t.TempDir()
	// Forced rather than measured: the floor must be exercised against a known number,
	// not against whatever the machine running the tests happens to have free.
	cfg.Data.FreeSpaceFloorBytes = 0

	if err := VerifyDataRoot(t.Context(), &cfg); err != nil {
		t.Fatalf("VerifyDataRoot: %v", err)
	}
}

func TestDataRootRefusedBelowTheFreeSpaceFloor(t *testing.T) {
	cfg := Defaults()
	cfg.Data.Root = t.TempDir()
	cfg.Data.FreeSpaceFloorBytes = math.MaxInt64

	err := VerifyDataRoot(t.Context(), &cfg)
	if err == nil {
		t.Fatal("VerifyDataRoot accepted a directory below the floor")
	}
	if !strings.Contains(err.Error(), "free_space_floor_bytes") {
		t.Errorf("refusal does not name the setting that would relax it:\n%v", err)
	}
}

func TestDataRootRefusedWhenNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the mode bits this test sets")
	}
	cfg := Defaults()
	cfg.Data.Root = t.TempDir()
	cfg.Data.FreeSpaceFloorBytes = 0
	if err := os.Chmod(cfg.Data.Root, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.Data.Root, 0o700) })

	err := VerifyDataRoot(t.Context(), &cfg)
	if err == nil {
		t.Fatal("VerifyDataRoot accepted a read-only data.root")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("refusal does not name the problem:\n%v", err)
	}
}
