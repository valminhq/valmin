package control

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/store"
)

// Asserts a config written by a setup restore or a definition import keeps its value through
// the settle a later start runs, even when an older edit's pending copy was left on disk.
func TestAWrittenConfigSurvivesAStalePendingCopy(t *testing.T) {
	t.Parallel()
	const (
		name     = "plugin.cfg"
		restored = "[General]\nValue = restored\n"
		stale    = "[General]\nValue = stale\n"
	)
	tests := []struct {
		name  string
		write func(t *testing.T, inst *store.Instance)
	}{
		{"definition import", func(t *testing.T, inst *store.Instance) {
			t.Helper()
			if err := ApplyManifestConfigs(inst, []ManifestConfig{{File: name, Content: restored}}, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"setup restore", func(t *testing.T, inst *store.Instance) {
			t.Helper()
			staging := t.TempDir()
			configs := []ManifestConfig{{File: name, Content: restored}}
			if err := writeSetupTargetConfigs(staging, configs); err != nil {
				t.Fatal(err)
			}
			target, err := SetupPaths(nil, configs)
			if err != nil {
				t.Fatal(err)
			}
			current := SetupFiles{SetupRoot{}: {instance.ConfigDir + "/" + name: true}}
			if err := ApplySetupPaths(inst, staging, current, target); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst := &store.Instance{ID: "inst-a", DataDir: t.TempDir()}
			dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(instance.ConfigDir))
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			for file, content := range map[string]string{name: stale, name + modconfig.PendingSuffix: stale} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			tc.write(t, inst)
			if err := settleIn(inst.DataDir); err != nil {
				t.Fatal(err)
			}

			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != restored {
				t.Errorf("config after settle = %q, want %q", got, restored)
			}
		})
	}
}
