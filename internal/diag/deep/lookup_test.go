package deep

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/runtime"
)

// A resolved name passes, a name the resolvers cannot answer warns, and anything else is
// unknown.
func TestProbeLookupGradesEachOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		createErr error
		status    diag.Status
		detail    string
	}{
		{name: "resolved", code: 0, status: diag.StatusOK, detail: "resolved"},
		{name: "not found", code: exitNotFound, status: diag.StatusWarn, detail: "could not resolve"},
		{name: "timeout", code: exitTimedOut, status: diag.StatusWarn, detail: "could not resolve"},
		{name: "other failure", code: 127, status: diag.StatusUnknown, detail: "without a recognised answer"},
		{name: "runtime refused", createErr: errors.New("unknown runtime"), status: diag.StatusUnknown, detail: "could not run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := runtime.NewFake()
			rt.CreateErr = tc.createErr
			rt.OnStart = func(c *runtime.FakeContainer) { c.Exit(tc.code) }
			cfg := config.Defaults()

			obs := probeLookup(context.Background(), rt, &cfg)

			if obs.Status != tc.status || !strings.Contains(obs.Detail, tc.detail) {
				t.Errorf("probe = %s %q, want %s containing %q", obs.Status, obs.Detail, tc.status, tc.detail)
			}
			if obs.OK != (tc.status == diag.StatusOK) {
				t.Errorf("OK = %v with status %s", obs.OK, obs.Status)
			}
			if obs.Target != diag.LookupTarget(&cfg) {
				t.Errorf("target = %q, want %q", obs.Target, diag.LookupTarget(&cfg))
			}
		})
	}
}

// The lookup runs as a game server would: under game.runtime, on the game network, with the
// game resolv.conf mounted.
func TestProbeLookupRunsAsAGameServer(t *testing.T) {
	rt := runtime.NewFake()
	var spec runtime.ContainerSpec
	rt.OnStart = func(c *runtime.FakeContainer) {
		spec = c.Spec
		c.Exit(0)
	}
	cfg := config.Defaults()
	cfg.Game.Runtime, cfg.Game.DNS = "kata", []string{"192.168.1.1"}
	cfg.Data.HostRoot = "/host/valmin"

	probeLookup(context.Background(), rt, &cfg)

	if spec.OCIRuntime != "kata" || spec.Network != cfg.Game.Network || spec.Image != cfg.Game.Image {
		t.Errorf("probe = runtime %q network %q image %q, want the game container's",
			spec.OCIRuntime, spec.Network, spec.Image)
	}
	if !slices.Equal(spec.Binds, cfg.GameResolvBinds()) {
		t.Errorf("binds = %+v, want the game resolv.conf %+v", spec.Binds, cfg.GameResolvBinds())
	}
	if !slices.Contains(spec.Entrypoint, lookupName) {
		t.Errorf("entrypoint = %q, want it to look up %s", spec.Entrypoint, lookupName)
	}
}
