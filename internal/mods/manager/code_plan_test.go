package manager

import (
	"errors"
	"strings"
	"testing"

	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/store/storetest"
)

const codeInstance = "inst-code"

// codeWorld is an instance holding mods from both registries, in a catalogue where a few
// packages exist in only one of them.
func codeWorld(t *testing.T) *Planner {
	t.Helper()
	db := storetest.Open(t)
	_, err := db.Writer.ExecContext(t.Context(), `INSERT INTO instances
		(id, name, data_dir, base_port, server_name, world_name, password, public, crossplay,
		 crossplay_instance_id, state, mem_limit_mb, created_at, updated_at)
		VALUES (?, 'code', ?, 2456, 'S', 'W', 'enc', 0, 0, ?, 'stopped', 2048, ?, ?)`,
		codeInstance, t.TempDir(), codeInstance, store.Now(), store.Now())
	if err != nil {
		t.Fatal(err)
	}

	// package -> registry -> versions the registry carries.
	catalogue := map[string]map[source.Source][]string{
		"Dual-Mod":     {source.Thunderstore: {"1.0.0", "2.0.0"}, source.Hexium: {"2.0.0"}},
		"Hex-Only":     {source.Hexium: {"1.0.0"}},
		"Hex-Moved":    {source.Hexium: {"1.0.0"}, source.Thunderstore: {"2.0.0"}},
		"Down-Mod":     {source.Thunderstore: {"1.0.0", "2.0.0"}},
		"Same-Mod":     {source.Thunderstore: {"1.0.0"}},
		"Locked-Mod":   {source.Thunderstore: {"1.0.0", "2.0.0"}},
		"Off-Mod":      {source.Thunderstore: {"1.0.0", "2.0.0"}},
		BepInExPack:    {source.Thunderstore: {"5.4.2333", "5.4.2350"}},
		"New-Both":     {source.Thunderstore: {"1.0.0"}, source.Hexium: {"1.0.0"}},
		"New-TsOnly":   {source.Thunderstore: {"1.0.0"}},
		"New-HexOnly":  {source.Hexium: {"1.0.0"}},
		"Pack-Mod":     {source.Thunderstore: {"1.0.0", "2.0.0"}},
		"Pack-Content": {source.Thunderstore: {"1.0.0", "2.0.0"}},
	}
	for name, perSource := range catalogue {
		for src, versions := range perSource {
			vs := make([]store.ModVersion, 0, len(versions))
			for _, v := range versions {
				vs = append(vs, store.ModVersion{Source: src, FullName: name, Version: v, DependenciesJSON: "[]"})
			}
			pkg := store.ModPackage{
				Source: src, FullName: name, Namespace: "N", Name: strings.TrimPrefix(name, "N-"),
				LatestVersion: versions[len(versions)-1],
			}
			if name == "Pack-Mod" {
				pkg.CategoriesJSON = `["Modpacks"]`
			}
			if err := db.UpsertModPackages(t.Context(), []store.ModPackage{pkg}, vs); err != nil {
				t.Fatal(err)
			}
		}
	}

	rows := []struct {
		name, version string
		src           source.Source
		enabled       bool
		locked        bool
	}{
		{"Dual-Mod", "1.0.0", source.Thunderstore, true, false},
		{"Hex-Only", "1.0.0", source.Hexium, true, false},
		{"Hex-Moved", "1.0.0", source.Hexium, true, false},
		{"Down-Mod", "2.0.0", source.Thunderstore, true, false},
		{"Same-Mod", "1.0.0", source.Thunderstore, true, false},
		{"Locked-Mod", "1.0.0", source.Thunderstore, true, true},
		{"Off-Mod", "1.0.0", source.Thunderstore, false, false},
		{BepInExPack, "5.4.2333", source.Thunderstore, true, false},
		{"Pack-Mod", "1.0.0", source.Thunderstore, true, false},
	}
	for _, r := range rows {
		_, err := db.Writer.ExecContext(t.Context(), `INSERT INTO instance_mods
			(instance_id, full_name, source, version, installed_as, side, enabled, locked, file_manifest, installed_at)
			VALUES (?, ?, ?, ?, 'explicit', 'unknown', ?, ?, '[]', ?)`,
			codeInstance, r.name, r.src.String(), r.version, r.enabled, r.locked, store.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	return &Planner{DB: db, Enabled: source.All()}
}

// TestClassifyCode asserts what applying a code does to each kind of package, above all that an
// installed package never changes registry.
func TestClassifyCode(t *testing.T) {
	t.Parallel()
	p := codeWorld(t)
	mods := []CodeMod{
		{FullName: "Dual-Mod", Version: "2.0.0"},
		{FullName: "Hex-Only", Version: "2.0.0"},
		{FullName: "Hex-Moved", Version: "2.0.0", Source: "thunderstore"},
		{FullName: "Down-Mod", Version: "1.0.0"},
		{FullName: "Same-Mod", Version: "1.0.0"},
		{FullName: "Locked-Mod", Version: "2.0.0"},
		{FullName: "Off-Mod", Version: "2.0.0"},
		{FullName: BepInExPack, Version: "5.4.2350"},
		{FullName: "Pack-Mod", Version: "2.0.0"},
		{FullName: "New-Both", Version: "1.0.0"},
		{FullName: "New-TsOnly", Version: "1.0.0"},
		{FullName: "New-HexOnly", Version: "1.0.0"},
		{FullName: "New-Nowhere", Version: "1.0.0"},
		{FullName: "New-Named", Version: "1.0.0", Source: "hexium"},
		{FullName: "New-Unknown", Version: "1.0.0", Source: "elsewhere"},
		{FullName: "Dual-Mod", Version: "1.0.0"},
	}
	tests := []struct {
		origin source.Source
		want   map[string][3]string // change, skip, source
	}{
		{source.Thunderstore, map[string][3]string{
			"Dual-Mod":     {ChangeUpgrade, "", "thunderstore"},
			"Hex-Only":     {"", SkipNotInRegistry, "hexium"},
			"Hex-Moved":    {"", SkipOtherRegistry, "hexium"},
			"Down-Mod":     {ChangeDowngrade, "", "thunderstore"},
			"Locked-Mod":   {"", SkipLocked, "thunderstore"},
			"Off-Mod":      {"", SkipDisabled, "thunderstore"},
			BepInExPack:    {"", SkipFramework, "thunderstore"},
			"Pack-Mod":     {"", SkipModpack, "thunderstore"},
			"New-Both":     {ChangeInstall, "", "thunderstore"},
			"New-TsOnly":   {ChangeInstall, "", "thunderstore"},
			"New-HexOnly":  {ChangeInstall, "", "hexium"},
			"New-Nowhere":  {"", SkipUnavailable, ""},
			"New-Named":    {"", SkipUnavailable, "hexium"},
			"New-Unknown":  {"", SkipUnknownRegistry, "elsewhere"},
			"Same-Mod":     {},
			"Pack-Content": {},
		}},
		{source.Hexium, map[string][3]string{
			"Dual-Mod":    {ChangeUpgrade, "", "thunderstore"},
			"New-Both":    {ChangeInstall, "", "hexium"},
			"New-TsOnly":  {ChangeInstall, "", "thunderstore"},
			"New-HexOnly": {ChangeInstall, "", "hexium"},
		}},
	}
	for _, tt := range tests {
		t.Run("served by "+tt.origin.String(), func(t *testing.T) {
			rows, matching, err := p.ClassifyCode(t.Context(), codeInstance, mods, tt.origin)
			if err != nil {
				t.Fatal(err)
			}
			if matching != 1 {
				t.Errorf("matching = %d, want only Same-Mod", matching)
			}
			got := map[string]CodeRow{}
			for _, r := range rows {
				got[r.FullName] = r
			}
			if len(got) != len(rows) {
				t.Errorf("a package is listed twice: %+v", rows)
			}
			for name, want := range tt.want {
				row, ok := got[name]
				if want == ([3]string{}) {
					if ok {
						t.Errorf("%s listed as %+v, want it absent", name, row)
					}
					continue
				}
				if !ok {
					t.Errorf("%s missing from %+v", name, rows)
					continue
				}
				if row.Change != want[0] || row.Skip != want[1] || row.Source != want[2] {
					t.Errorf("%s = change %q skip %q source %q, want %q", name, row.Change, row.Skip, row.Source, want)
				}
			}
		})
	}
}

// TestCodeTargetsFollowTheSelection asserts a downgrade is only a target when it is chosen.
func TestCodeTargetsFollowTheSelection(t *testing.T) {
	t.Parallel()
	rows := []CodeRow{
		{FullName: "Up", Installed: "1.0.0", Version: "2.0.0", Source: "thunderstore", Change: ChangeUpgrade},
		{FullName: "Down", Installed: "2.0.0", Version: "1.0.0", Source: "thunderstore", Change: ChangeDowngrade},
		{FullName: "New", Version: "1.0.0", Source: "hexium", Change: ChangeInstall},
		{FullName: "Skipped", Version: "1.0.0", Skip: SkipLocked},
	}
	got := CodeTargets(rows, DefaultCodeSelection)
	if len(got) != 2 || got[0].FullName != "Up" || got[1].FullName != "New" || got[1].Source != "hexium" {
		t.Errorf("default targets = %+v, want Up and New", got)
	}
	all := CodeTargets(rows, func(*CodeRow) bool { return true })
	if len(all) != 3 {
		t.Errorf("targets with everything chosen = %+v, want the three actionable rows", all)
	}
}

// TestPlanUpdatesKeepsEachTargetOnItsRegistry asserts a batch of installs, raises and lowers
// resolves every package from the registry it names, and that an installed package is never
// moved to another one.
func TestPlanUpdatesKeepsEachTargetOnItsRegistry(t *testing.T) {
	t.Parallel()
	p := codeWorld(t)
	targets := []UpdateTarget{
		{FullName: "Dual-Mod", Source: "thunderstore", FromVersion: "1.0.0", Version: "2.0.0"},
		{FullName: "Down-Mod", Source: "thunderstore", FromVersion: "2.0.0", Version: "1.0.0"},
		{FullName: "New-Both", Source: "hexium", Version: "1.0.0"},
	}
	idx := NewIndex(t.Context(), p.DB, codeInstance, source.Thunderstore, p.Enabled)
	plan, err := PlanUpdates(targets, idx)
	if err != nil || idx.Err() != nil {
		t.Fatalf("PlanUpdates: %v, index: %v", err, idx.Err())
	}
	got := map[string]string{}
	for _, n := range plan.Closure.Nodes {
		got[n.FullName] = idx.SourceOf(n.FullName, n.Version).String()
	}
	for name, want := range map[string]string{"Dual-Mod": "thunderstore", "Down-Mod": "thunderstore", "New-Both": "hexium"} {
		if got[name] != want {
			t.Errorf("%s resolved from %q, want %q (nodes %v)", name, got[name], want, got)
		}
	}

	moved := []UpdateTarget{{FullName: "Hex-Moved", Source: "thunderstore", FromVersion: "1.0.0", Version: "2.0.0"}}
	idx = NewIndex(t.Context(), p.DB, codeInstance, source.Thunderstore, p.Enabled)
	_, err = PlanUpdates(moved, idx)
	if _, ok := errors.AsType[*modresolver.UnresolvedError](err); !ok {
		t.Errorf("moving an installed Hexium mod to Thunderstore: error = %v, want it unresolved", err)
	}
}

// TestCheckCodeTargets asserts the apply gate: a target may install or move either way, and
// anything that would switch a registry or touch a protected package is refused.
func TestCheckCodeTargets(t *testing.T) {
	t.Parallel()
	p := codeWorld(t)
	tests := []struct {
		name   string
		target UpdateTarget
		want   string
	}{
		{"install", UpdateTarget{FullName: "New-TsOnly", Source: "thunderstore", Version: "1.0.0"}, ""},
		{"raise", UpdateTarget{FullName: "Dual-Mod", Source: "thunderstore", Version: "2.0.0"}, ""},
		{"lower", UpdateTarget{FullName: "Down-Mod", Source: "thunderstore", Version: "1.0.0"}, ""},
		{
			"another registry",
			UpdateTarget{FullName: "Hex-Moved", Source: "thunderstore", Version: "2.0.0"},
			"changes there only",
		},
		{"same version", UpdateTarget{FullName: "Same-Mod", Source: "thunderstore", Version: "1.0.0"}, "already at"},
		{"locked", UpdateTarget{FullName: "Locked-Mod", Source: "thunderstore", Version: "2.0.0"}, "locked"},
		{"disabled", UpdateTarget{FullName: "Off-Mod", Source: "thunderstore", Version: "2.0.0"}, "disabled"},
		{"framework", UpdateTarget{FullName: BepInExPack, Source: "thunderstore", Version: "5.4.2350"}, "framework"},
		{"modpack", UpdateTarget{FullName: "Pack-Mod", Source: "thunderstore", Version: "2.0.0"}, "modpack"},
		{
			"unknown registry",
			UpdateTarget{FullName: "New-TsOnly", Source: "elsewhere", Version: "1.0.0"},
			"no mod registry",
		},
		{"no registry", UpdateTarget{FullName: "New-TsOnly", Version: "1.0.0"}, "no mod registry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, issues, err := p.CheckCodeTargets(t.Context(), codeInstance, []UpdateTarget{tt.target})
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(issues) != 0 || len(out) != 1 {
					t.Errorf("issues = %+v, out = %+v, want the target accepted", issues, out)
				}
				return
			}
			if len(issues) != 1 || !strings.Contains(issues[0].Message, tt.want) {
				t.Errorf("issues = %+v, want one containing %q", issues, tt.want)
			}
		})
	}

	_, issues, err := p.CheckCodeTargets(t.Context(), codeInstance, []UpdateTarget{
		{FullName: "New-TsOnly", Source: "thunderstore", Version: "1.0.0"},
		{FullName: "New-TsOnly", Source: "thunderstore", Version: "1.0.0"},
	})
	if err != nil || len(issues) != 1 || !strings.Contains(issues[0].Message, "twice") {
		t.Errorf("a repeated target: issues = %+v, err = %v, want one 'twice'", issues, err)
	}
	if _, issues, _ := p.CheckCodeTargets(t.Context(), codeInstance, nil); len(issues) != 1 {
		t.Errorf("no targets: issues = %+v, want one", issues)
	}
}

// TestClassifyCodeNeverInstallsTheFramework asserts the mod framework is left to the panel even
// when the server has none.
func TestClassifyCodeNeverInstallsTheFramework(t *testing.T) {
	t.Parallel()
	p := codeWorld(t)
	if _, err := p.DB.Writer.ExecContext(t.Context(),
		`DELETE FROM instance_mods WHERE instance_id = ? AND full_name = ?`, codeInstance, BepInExPack); err != nil {
		t.Fatal(err)
	}
	rows, _, err := p.ClassifyCode(t.Context(), codeInstance,
		[]CodeMod{{FullName: BepInExPack, Version: "5.4.2350"}}, source.Thunderstore)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Skip != SkipFramework || rows[0].Actionable() {
		t.Errorf("rows = %+v, want the framework left alone", rows)
	}
}

// TestCheckCodeTargetsRefusesTooMany asserts the size of one change is bounded.
func TestCheckCodeTargetsRefusesTooMany(t *testing.T) {
	t.Parallel()
	p := codeWorld(t)
	targets := make([]UpdateTarget, MaxCodeTargets+1)
	for i := range targets {
		targets[i] = UpdateTarget{
			FullName: "N-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)),
			Source:   "thunderstore",
			Version:  "1.0.0",
		}
	}
	_, issues, err := p.CheckCodeTargets(t.Context(), codeInstance, targets)
	if err != nil || len(issues) != 1 || !strings.Contains(issues[0].Message, "Too many") {
		t.Errorf("issues = %+v, err = %v, want one 'Too many'", issues, err)
	}
}
