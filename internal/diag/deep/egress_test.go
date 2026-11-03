package deep

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/runtime"
)

// refusedStderr is what the game image's bash printed for a refused connect.
func refusedStderr(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/refused.stderr")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each outcome of a connection attempt is graded honestly: an answer of any kind is a
// warning, silence passes with its caveat, and anything else is unknown.
func TestProbeEgressGradesEachOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		stderr    string
		createErr error
		status    diag.Status
		detail    string
	}{
		{name: "connected", code: 0, status: diag.StatusWarn, detail: "opened a connection"},
		{name: "refused", code: 1, stderr: refusedStderr(t), status: diag.StatusWarn, detail: "refused the connection"},
		{name: "timeout", code: exitTimedOut, status: diag.StatusOK, detail: "not proof that it is blocked"},
		{name: "other failure", code: 1, stderr: "bash: connect: No route to host\n", status: diag.StatusUnknown, detail: "without a recognised answer"},
		{name: "image missing", createErr: errors.New("no such image"), status: diag.StatusUnknown, detail: "could not run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.NewFake()
			rt.CreateErr = tc.createErr
			rt.OnStart = func(c *runtime.FakeContainer) {
				c.Stderr(tc.stderr)
				c.Exit(tc.code)
			}
			cfg := config.Defaults()
			target := lanTarget(netip.MustParseAddrPort("192.168.1.1:80"))

			obs := probeEgress(context.Background(), rt, &cfg, target)

			if obs.Status != tc.status || !strings.Contains(obs.Detail, tc.detail) {
				t.Errorf("probe = %s %q, want %s containing %q", obs.Status, obs.Detail, tc.status, tc.detail)
			}
			if obs.OK != (tc.status == diag.StatusOK) {
				t.Errorf("OK = %v with status %s", obs.OK, obs.Status)
			}
			// The detail goes into the support bundle, so the LAN address and verbatim output
			// stay out of it.
			if strings.Contains(obs.Detail, "192.168.1.1") || strings.Contains(obs.Detail, "No route") {
				t.Errorf("detail %q carries the address or the probe's output", obs.Detail)
			}
			if tc.createErr == nil && obs.Target != "192.168.1.1:80" {
				t.Errorf("target = %q, want the probed address", obs.Target)
			}
		})
	}
}

// The address reaches the probe as arguments, never inside the script, and the container
// joins the game network and is removed afterwards.
func TestProbeEgressPassesTheAddressAsArguments(t *testing.T) {
	rt := runtime.NewFake()
	var spec runtime.ContainerSpec
	rt.OnStart = func(c *runtime.FakeContainer) {
		spec = c.Spec
		c.Exit(exitTimedOut)
	}
	cfg := config.Defaults()
	cfg.Game.Network = "valmin-games"

	probeEgress(context.Background(), rt, &cfg, &metadataTarget)

	want := []string{"/usr/bin/timeout", "3", "/bin/bash", "-c", "</dev/tcp/$0/$1", "169.254.169.254", "80"}
	if !slices.Equal(spec.Entrypoint, want) {
		t.Errorf("entrypoint = %q, want %q", spec.Entrypoint, want)
	}
	if spec.Network != "valmin-games" || spec.Image != cfg.Game.Image {
		t.Errorf("probe ran %s on network %q, want the game image on valmin-games", spec.Image, spec.Network)
	}
	if left, err := rt.List(context.Background(), nil); err != nil || len(left) != 0 {
		t.Errorf("containers left after the probe: %d (%v)", len(left), err)
	}
}

// Classification needs the measured refusal literal; any other exit-1 message is unclear.
func TestClassifyEgress(t *testing.T) {
	for _, tc := range []struct {
		code   int
		stderr string
		want   egressResult
	}{
		{0, "", egressConnected},
		{1, refusedStderr(t), egressRefused},
		{1, "bash: router: Name or service not known\n", egressUnclear},
		{exitTimedOut, "", egressNoAnswer},
		{127, "timeout: failed to run command\n", egressUnclear},
	} {
		if got := classifyEgress(tc.code, tc.stderr); got != tc.want {
			t.Errorf("classifyEgress(%d, %q) = %d, want %d", tc.code, tc.stderr, got, tc.want)
		}
	}
}
