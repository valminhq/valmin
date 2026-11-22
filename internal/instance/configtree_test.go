package instance

import "testing"

// TestCheckConfigPath asserts it accepts a relative path into a subdirectory and refuses
// anything that escapes, hides, nests too deep or is not a config file.
func TestCheckConfigPath(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"com.example.mod.cfg",
		"expand_world/expand_biomes.yaml",
		"wackysDatabase/Items/a/b.yml",
		"EpicLoot/patches/x.json",
		"notes.txt",
		"a/b/c/d/e/f/g/h/ok.yaml",
		".hidden.cfg",
	} {
		if err := CheckConfigPath(name); err != nil {
			t.Errorf("CheckConfigPath(%q) = %v, want accepted", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..", "/x.yaml", "a/../b.yaml", "a//b.yaml", "a/b.yaml/",
		`a\b.yaml`, ".hidden/x.yaml", "a/.x/b.yaml", "x.png", "x.yaml.bak", "x.cfg.pending",
		"a/b/c/d/e/f/g/h/i/deep.yaml",
	} {
		if err := CheckConfigPath(name); err == nil {
			t.Errorf("CheckConfigPath(%q) accepted, want refused", name)
		}
	}
}
