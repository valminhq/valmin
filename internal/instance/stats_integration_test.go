//go:build integration

package instance_test

import (
	"errors"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	runtimepkg "github.com/valminhq/valmin/internal/runtime"
)

// TestStatsAgainstARealContainer covers what the fake cannot: real cgroup counters, a real
// memory limit, and two samples far enough apart for the delta to mean something.
func TestStatsAgainstARealContainer(t *testing.T) {
	d := dockerRuntime(t)
	spec := rawStubSpec(t, "wp20-stats")
	spec.MemoryBytes = 512 << 20
	id := create(t, d, spec)
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Let the container settle before sampling. Measured: a freshly started
	// stub's memory falls from 2.2 MB to 0.5 MB over its first five seconds as page cache is
	// reclaimed, and comparing two readings taken moments apart during that window compares
	// noise. Once settled, the panel and `docker stats` agree to the byte.
	time.Sleep(5 * time.Second)

	// Give the container some page cache before comparing. Without it `inactive_file` is
	// near zero, `memory.current` and the working set are the same number, and the parity
	// check passes just as happily with E11's subtraction deleted — measured by
	// deleting it. A check that cannot fail is the failure mode CLAUDE.md §7 warns about.
	fillPageCache(t, id)

	streams := instance.NewStreams(d)
	defer streams.Shutdown()
	streams.Open("wp20", id)

	samples, cancel := streams.Sampler("wp20").Subscribe()
	defer cancel()

	first := next(t, samples)
	if first.CPUPct != nil {
		t.Errorf("the first sample carries cpu_pct = %v, want nil (E10)", *first.CPUPct)
	}
	if first.MemBytes == 0 || first.MemLimit == 0 {
		t.Fatalf("memory reads as %d / %d against a real container", first.MemBytes, first.MemLimit)
	}
	if first.MemLimit != 512<<20 {
		t.Errorf("mem_limit = %d, want the container's own 512 MiB limit, not the host's RAM (E11)",
			first.MemLimit)
	}
	if first.Players != nil {
		t.Errorf("players = %d, want nil (E7)", *first.Players)
	}

	second := next(t, samples)
	if second.CPUPct == nil {
		t.Fatal("the second sample carries no cpu_pct")
	}
	// An idle stub is near zero, but the number must be a real one in a sane range rather
	// than a nonsense delta.
	if *second.CPUPct < 0 || *second.CPUPct > 100*float64(runtime.NumCPU()) {
		t.Errorf("cpu_pct = %v is not a plausible percentage", *second.CPUPct)
	}

	assertMatchesDockerStats(t, d, id)
}

// assertMatchesDockerStats is the parity half of E11: whatever the panel shows must be the
// number the operator sees in `docker stats`, or one of the two is wrong and neither is
// trustworthy. Q24 measured this once; this asserts it still holds.
//
// It takes its own reading immediately after the CLI's rather than using the sampler's,
// which can be a full SampleInterval old. Measured: an idle container's memory
// still moves by ~256 KiB over two seconds, so comparing across that gap measures drift, not
// arithmetic — the check failed one run in three that way. The sampler's pass-through of
// this number is asserted in the unit tests instead.
func assertMatchesDockerStats(t *testing.T, rt runtimepkg.Runtime, containerID string) {
	t.Helper()
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker CLI on PATH: the parity check needs it, the rest of the test does not")
	}
	// Best of five, and that is not papering over a flake. A container's memory genuinely
	// moves between two readings taken milliseconds apart — measured at up to 430 KiB while
	// the kernel reclaims the cache warmed above — so a single pair sometimes disagrees for
	// reasons that have nothing to do with the arithmetic. One agreeing pair proves the
	// formula; a wrong formula disagrees on *every* attempt, which is what the control that
	// deletes E11's subtraction shows (48 MiB out, five times out of five).
	var lastGot uint64
	var lastWant float64
	for range 5 {
		out, err := exec.Command(cli, "stats", "--no-stream", "--format", "{{.MemUsage}}", containerID).Output()
		if err != nil {
			t.Skipf("docker stats: %v", err)
		}
		want, err := parseMemUsage(string(out))
		if err != nil {
			t.Skipf("could not read %q: %v", out, err)
		}
		raw, err := rt.Stats(t.Context(), containerID)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}

		// `docker stats` prints three significant figures in its own unit, so half a unit of
		// the last digit — about 0.5% — is the whole tolerance. Measured: on a settled
		// container the two agree exactly (532KiB against 544768 bytes), so anything looser
		// would not notice a wrong figure. The 1 KiB floor is for the smallest units only.
		lastGot, lastWant = raw.MemBytes, want
		if math.Abs(float64(raw.MemBytes)-want) <= want*0.005+1024 {
			return
		}
	}
	t.Errorf("mem_bytes = %d, docker stats says %.0f (%.0f bytes apart) on every attempt",
		lastGot, lastWant, math.Abs(float64(lastGot)-lastWant))
}

// fillPageCache writes and re-reads a file inside the container, so its cgroup carries a
// real inactive_file term for the parity check to be sensitive to.
func fillPageCache(t *testing.T, containerID string) {
	t.Helper()
	cli, err := exec.LookPath("docker")
	if err != nil {
		return
	}
	out, err := exec.Command(cli, "exec", containerID, "sh", "-c",
		"dd if=/dev/urandom of=/tmp/cache bs=1M count=48 2>/dev/null && cat /tmp/cache > /dev/null").CombinedOutput()
	if err != nil {
		t.Logf("could not warm the page cache (%v): %s", err, out)
	}
}

// parseMemUsage reads the left half of `docker stats`'s "1.044GiB / 4GiB".
func parseMemUsage(s string) (float64, error) {
	used, _, _ := strings.Cut(strings.TrimSpace(s), "/")
	used = strings.TrimSpace(used)

	units := []struct {
		suffix string
		scale  float64
	}{
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"GB", 1e9},
		{"MB", 1e6},
		{"kB", 1e3},
		{"B", 1},
	}
	for _, u := range units {
		if rest, ok := strings.CutSuffix(used, u.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return 0, err
			}
			return n * u.scale, nil
		}
	}
	return 0, errors.New("unrecognised memory unit")
}

func next(t *testing.T, samples <-chan instance.Sample) instance.Sample {
	t.Helper()
	select {
	case s := <-samples:
		return s
	case <-time.After(instance.SampleInterval * 4):
		t.Fatal("no sample arrived")
		return instance.Sample{}
	}
}
