package control

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// TestApplyManifestConfigsWritesNestedFiles asserts an imported config file at a path under the
// config directory is written there, its folders created, and that a merge replaces a file other
// than .cfg whole while it merges a .cfg's settings.
func TestApplyManifestConfigsWritesNestedFiles(t *testing.T) {
	t.Parallel()
	inst := &store.Instance{ID: "inst-a", DataDir: t.TempDir()}
	db := instanceDB(t, inst)
	dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(instance.ConfigDir))
	if err := os.MkdirAll(filepath.Join(dir, "EWD"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"EWD/a.yaml": "a: 1\nb: 1\n",
		"Plugin.cfg": "[A]\nkeep = 1\nk = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configs := []ManifestConfig{
		{File: "EWD/a.yaml", Content: "a: 2\n"},
		{File: "New/deep/b.json", Content: "{}\n"},
		{File: "Plugin.cfg", Content: "[A]\nk = 2\n"},
	}
	if err := ApplyManifestConfigs(t.Context(), db, inst, configs, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"EWD/a.yaml":      "a: 2\n",
		"New/deep/b.json": "{}\n",
		"Plugin.cfg":      "[A]\nkeep = 1\nk = 2\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "New", "deep"))
	if err != nil {
		t.Fatal(err)
	}
	const bits = fs.ModeSetgid | fs.ModePerm
	if got := info.Mode() & bits; got != fsutil.DirMode&bits {
		t.Errorf("created folder mode = %v, want %v", got, fsutil.DirMode&bits)
	}
	if err := ApplyManifestConfigs(t.Context(), db, inst,
		[]ManifestConfig{{File: "../escape.yaml", Content: "x"}}, false); err == nil {
		t.Error("a path out of the config directory was written")
	}
}
