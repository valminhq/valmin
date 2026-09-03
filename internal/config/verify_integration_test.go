//go:build integration

package config

import (
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
)

// stubImage stands in for game.image. Using it rather than pulling one keeps the suite
// hermetic (06 §4), and it exercises the decision behind ADR-048: the check runs whatever
// image the panel already requires, so it needs no registry of its own.
const stubImage = "valmin/valheim-stub:dev"

func dockerConfig(t *testing.T, hostRoot string) (*Config, *runtime.Docker) {
	t.Helper()
	d, err := runtime.NewDocker(t.Context(), "unix:///var/run/docker.sock", "")
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	cfg := Defaults()
	cfg.Game.Image = stubImage
	cfg.Data.Root = t.TempDir()
	cfg.Data.HostRoot = hostRoot
	if hostRoot == "" {
		cfg.Data.HostRoot = cfg.Data.Root
	}
	return &cfg, d
}

// The host_data_root round trip against a real daemon: a token written by the panel and
// read back through a bind mount of data.host_root.
func TestVerifyHostRootRoundTripsThroughARealContainer(t *testing.T) {
	cfg, d := dockerConfig(t, "")

	if err := VerifyHostRoot(t.Context(), d, cfg); err != nil {
		t.Fatalf("VerifyHostRoot: %v", err)
	}
}

func TestVerifyHostRootRefusesAWrongHostRootAgainstARealDaemon(t *testing.T) {
	cfg, d := dockerConfig(t, t.TempDir())

	err := VerifyHostRoot(t.Context(), d, cfg)
	if err == nil {
		t.Fatal("VerifyHostRoot accepted a data.host_root that mounts another directory")
	}
	for _, want := range []string{cfg.Data.Root, cfg.Data.HostRoot, "path on the host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// Q27's constraint against the daemon rather than the fake: nothing pulls the image, so a
// missing one must refuse rather than skip (C22).
func TestVerifyHostRootRefusesWhenTheImageIsNotPresent(t *testing.T) {
	cfg, d := dockerConfig(t, "")
	cfg.Game.Image = "valmin/definitely-not-pulled:dev"

	err := VerifyHostRoot(t.Context(), d, cfg)
	if err == nil {
		t.Fatal("VerifyHostRoot passed with no image to run")
	}
	if !strings.Contains(err.Error(), cfg.Game.Image) {
		t.Errorf("refusal does not name the missing image:\n%v", err)
	}
}
