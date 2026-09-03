package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bootLog is the measured boot sequence (03 §5.3), with the padding BepInEx actually emits.
const bootLog = `[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server (8/20/2026 9:14:02 PM)
[Message:   BepInEx] Preloader started
[Info   :   BepInEx] Patching [UnityEngine.CoreModule] with [BepInEx.Chainloader]
[Message:   BepInEx] Chainloader ready
[Message:   BepInEx] Chainloader started
[Info   :   BepInEx] 2 plugins to load
[Info   :   BepInEx] Loading [Jotunn 2.29.2]
[Info   :   BepInEx] Loading [Sailing 1.1.8]
[Message:   BepInEx] Chainloader startup complete
`

func writeBepInExLog(t *testing.T, body string) string {
	t.Helper()
	dataDir := t.TempDir()
	p := filepath.Join(dataDir, filepath.FromSlash(bepinexLog))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// TestReadPluginLoadCountsWhatWasNamed is 03 §5.3's preferred order: the per-plugin lines
// are counted, and the count line is kept beside them as a cross-check rather than trusted
// over them.
func TestReadPluginLoadCountsWhatWasNamed(t *testing.T) {
	load, err := ReadPluginLoad(writeBepInExLog(t, bootLog))
	if err != nil {
		t.Fatal(err)
	}
	if load == nil {
		t.Fatal("ReadPluginLoad = nil for a log that exists")
	}
	if load.Declared != 2 || len(load.Plugins) != 2 {
		t.Fatalf("declared %d, named %d; want 2 and 2", load.Declared, len(load.Plugins))
	}
	if got := load.Plugins[0]; got.Name != "Jotunn" || got.Version != "2.29.2" {
		t.Errorf("first plugin = %+v, want Jotunn 2.29.2", got)
	}
	if d := load.Discrepancy(); d != "" {
		t.Errorf("Discrepancy = %q on a log that agrees with itself", d)
	}
	if load.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero; the boot these results come from is not identified")
	}
}

// TestReadPluginLoadHandlesTheSingularCountLine is E9, at this level as well as in the
// pattern set: exactly one plugin logs "plugin", and the symptom of a pattern without the
// `?` is a blank indicator and no error at all.
func TestReadPluginLoadHandlesTheSingularCountLine(t *testing.T) {
	body := "[Info   :   BepInEx] 1 plugin to load\n[Info   :   BepInEx] Loading [Sailing 1.1.8]\n"
	load, err := ReadPluginLoad(writeBepInExLog(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if load.Declared != 1 {
		t.Errorf("declared = %d, want 1 — the singular count line was not matched", load.Declared)
	}
	if !load.Loaded("Smoothbrain-Sailing", []string{"BepInEx/plugins/Smoothbrain-Sailing/Sailing.dll"}) {
		t.Error("the one installed plugin did not report loaded")
	}
	if d := load.Discrepancy(); d != "" {
		t.Errorf("Discrepancy = %q; one declared and one named agree", d)
	}
}

// TestDiscrepancyIsReportedNotResolved: when BepInEx says it will load three plugins and
// names two, the panel says so. Resolving toward either number hides the one case that
// matters — a plugin the chainloader meant to load and never did.
func TestDiscrepancyIsReportedNotResolved(t *testing.T) {
	body := strings.Replace(bootLog, "2 plugins to load", "3 plugins to load", 1)
	load, err := ReadPluginLoad(writeBepInExLog(t, body))
	if err != nil {
		t.Fatal(err)
	}
	d := load.Discrepancy()
	if d == "" {
		t.Fatal("Discrepancy = \"\" when the count line and the plugin lines disagree")
	}
	if !strings.Contains(d, "3") || !strings.Contains(d, "2") {
		t.Errorf("Discrepancy = %q; it must carry both numbers", d)
	}
	// Neither number was quietly adopted as the truth.
	if load.Declared != 3 || len(load.Plugins) != 2 {
		t.Errorf("declared %d, named %d; both must survive the disagreement",
			load.Declared, len(load.Plugins))
	}
}

// TestPluginLoadReadsOnlyTheMostRecentRun. Whether BepInEx truncates its log per boot or
// appends to it is not measured, and taking everything after the last count line is right
// either way — a previous boot's plugins must not be reported as this one's, which is the
// same failure the container-log scoping fixed for readiness.
func TestPluginLoadReadsOnlyTheMostRecentRun(t *testing.T) {
	load, err := ReadPluginLoad(writeBepInExLog(t, bootLog+
		"[Info   :   BepInEx] 1 plugin to load\n[Info   :   BepInEx] Loading [Sailing 1.1.8]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if load.Declared != 1 || len(load.Plugins) != 1 {
		t.Fatalf("declared %d, named %d; want the second run alone", load.Declared, len(load.Plugins))
	}
	if load.Loaded("ValheimModding-Jotunn", []string{"BepInEx/plugins/Jotunn.dll"}) {
		t.Error("a plugin from the previous boot was reported as loaded in this one")
	}
}

// TestReadPluginLoadOnAServerThatHasNoLog is the difference between "not loading" and "no
// information". A server that has not been started since a mod was installed has no
// chainloader run to read, and reporting its mods as not loading would be a false alarm on
// every fresh install.
func TestReadPluginLoadOnAServerThatHasNoLog(t *testing.T) {
	load, err := ReadPluginLoad(t.TempDir())
	if load != nil || err != nil {
		t.Errorf("ReadPluginLoad of a server with no BepInEx log = %+v, %v; want nil, nil", load, err)
	}
}

// TestLoadedMatchesAPackageToItsPlugin covers the heuristic and its limits, both stated:
// the name half of the full name and the base names of the `.dll` files the package's own
// manifest placed are everything the panel knows about what a plugin will call itself.
func TestLoadedMatchesAPackageToItsPlugin(t *testing.T) {
	load := &PluginLoad{Plugins: []LoadedPlugin{
		{Name: "Jotunn", Version: "2.29.2"},
		{Name: "AAACrafting", Version: "2.1.6"},
		{Name: "Some Spaced Name", Version: "1.0"},
	}}
	tests := []struct {
		name     string
		fullName string
		paths    []string
		want     bool
	}{
		{"by the name half", "ValheimModding-Jotunn", []string{"BepInEx/plugins/Jotunn.dll"}, true},
		{
			"by a dll whose name differs from the package", "Azumatt-Whatever",
			[]string{"BepInEx/plugins/Azumatt-Whatever/AAA_Crafting.dll"},
			true,
		},
		{"separators and case are not a difference", "Azumatt-AAA_Crafting", nil, true},
		{"a plugin name with spaces", "Ns-Some_Spaced_Name", nil, true},
		{"nothing named it", "Ns-Absent", []string{"BepInEx/plugins/Ns-Absent/Absent.dll"}, false},
		{
			"a different character is a different plugin", "Ns-Jotun",
			[]string{"BepInEx/plugins/Ns-Jotun/Jotun.dll"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := load.Loaded(tt.fullName, tt.paths); got != tt.want {
				t.Errorf("Loaded(%q, %v) = %v, want %v", tt.fullName, tt.paths, got, tt.want)
			}
		})
	}
}

// TestIsPluginExcludesWhatBepInExNeverLoads. The framework package puts its assemblies in
// BepInEx/core/ and is never named by a Loading line; calling it `not_seen` would be a
// permanent warning about the one package whose presence *is* the mod loader.
func TestIsPluginExcludesWhatBepInExNeverLoads(t *testing.T) {
	tests := map[string]struct {
		paths []string
		want  bool
	}{
		"the framework pack": {[]string{
			"BepInEx/core/BepInEx.Preloader.dll", "doorstop_libs/libdoorstop_x64.so", "winhttp.dll",
		}, false},
		"a config-only package": {[]string{"BepInEx/config/Thing/en.yml"}, false},
		"a plugin":              {[]string{"BepInEx/plugins/Ns-Thing/Thing.dll"}, true},
		"a plugin with assets": {[]string{
			"BepInEx/plugins/Ns-Thing/assets.bundle", "BepInEx/plugins/Ns-Thing/Thing.DLL",
		}, true},
		"nothing at all": {nil, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := IsPlugin(tt.paths); got != tt.want {
				t.Errorf("IsPlugin(%v) = %v, want %v", tt.paths, got, tt.want)
			}
		})
	}
}
