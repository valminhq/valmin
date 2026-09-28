package resolver

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// fakeIndex is a tiny in-memory Index for tests, keyed "fullName@version" for
// dependencies and "fullName" for installed versions and holds.
type fakeIndex struct {
	deps      map[string][]string
	installed map[string]string
	held      map[string]bool
}

func (f *fakeIndex) Held(fullName string) bool { return f.held[fullName] }

func (f *fakeIndex) Dependencies(fullName, version string) ([]string, bool) {
	deps, ok := f.deps[fullName+"@"+version]
	return deps, ok
}

func (f *fakeIndex) Installed(fullName string) (string, bool) {
	v, ok := f.installed[fullName]
	return v, ok
}

func nodeByName(t *testing.T, nodes []Node, fullName string) Node {
	t.Helper()
	for _, n := range nodes {
		if n.FullName == fullName {
			return n
		}
	}
	t.Fatalf("%s not in closure: %+v", fullName, nodes)
	return Node{}
}

// TestResolveThreeDeepTree is the real corpus's own shape: OdinArchitect requires Jotunn,
// which requires the BepInEx pack — the "installing a mod with a three-deep dependency
// tree" acceptance test.
func TestResolveThreeDeepTree(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"OdinPlus-OdinArchitect@1.7.0":          {"ValheimModding-Jotunn-2.29.2"},
		"ValheimModding-Jotunn@2.29.2":          {"denikson-BepInExPack_Valheim-5.4.2333"},
		"denikson-BepInExPack_Valheim@5.4.2333": {},
	}}

	closure, err := Resolve([]Request{{FullName: "OdinPlus-OdinArchitect", Version: "1.7.0"}}, idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want 3", closure.Nodes)
	}

	if n := nodeByName(t, closure.Nodes, "OdinPlus-OdinArchitect"); n.Transitive {
		t.Error("the explicitly requested package must not be marked transitive")
	}
	if n := nodeByName(t, closure.Nodes, "ValheimModding-Jotunn"); !n.Transitive || n.Version != "2.29.2" {
		t.Errorf("Jotunn = %+v", n)
	}
	if n := nodeByName(t, closure.Nodes, "denikson-BepInExPack_Valheim"); !n.Transitive || n.Version != "5.4.2333" {
		t.Errorf("BepInEx = %+v", n)
	}
}

// TestResolveDiamondPicksHighestRequestedVersion is the real corpus's diamond: Armory and
// Sailing each depend on the BepInEx pack, at different versions.
func TestResolveDiamondPicksHighestRequestedVersion(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"Therzie-Armory@1.3.1":                  {"denikson-BepInExPack_Valheim-5.4.2202"},
		"Smoothbrain-Sailing@1.1.8":             {"denikson-BepInExPack_Valheim-5.4.2333"},
		"denikson-BepInExPack_Valheim@5.4.2202": {},
		"denikson-BepInExPack_Valheim@5.4.2333": {},
	}}

	closure, err := Resolve([]Request{
		{FullName: "Therzie-Armory", Version: "1.3.1"},
		{FullName: "Smoothbrain-Sailing", Version: "1.1.8"},
	}, idx)
	if err != nil {
		t.Fatal(err)
	}

	bepinex := nodeByName(t, closure.Nodes, "denikson-BepInExPack_Valheim")
	if bepinex.Version != "5.4.2333" {
		t.Errorf("BepInEx version = %s, want 5.4.2333 (the higher of the two requested)", bepinex.Version)
	}
	if !bepinex.Transitive {
		t.Error("BepInEx was never requested directly, want Transitive = true")
	}
}

func TestResolveDetectsACycleAndDoesNotRecurseForever(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"A-A@1.0.0": {"B-B-1.0.0"},
		"B-B@1.0.0": {"A-A-1.0.0"},
	}}

	_, err := Resolve([]Request{{FullName: "A-A", Version: "1.0.0"}}, idx)
	var cyc *CycleError
	if !errors.As(err, &cyc) {
		t.Fatalf("err = %v, want *CycleError", err)
	}
	if len(cyc.Cycle) < 2 {
		t.Errorf("cycle = %v, too short to show the loop", cyc.Cycle)
	}
}

// TestResolveVerifiesEveryEdgesVersionEvenAfterExpansion guards the ordering inside walk:
// A and B both depend on C at different versions, and only the lower one is in the index.
// C is expanded when A's edge is walked, and B's edge then raises it to a version that
// does not exist. A gate placed ahead of the index lookup would skip that check and report
// C at a version nothing verified, leaving it to surface later as a failed download.
func TestResolveVerifiesEveryEdgesVersionEvenAfterExpansion(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"A-A@1.0.0": {"C-C-1.0.0"},
		"B-B@1.0.0": {"C-C-2.0.0"}, // 2.0.0 is deliberately absent from the index
		"C-C@1.0.0": {},
	}}

	_, err := Resolve([]Request{
		{FullName: "A-A", Version: "1.0.0"},
		{FullName: "B-B", Version: "1.0.0"},
	}, idx)

	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v, want *UnresolvedError for the unverified C-C-2.0.0", err)
	}
	if unresolved.Ident() != "C-C-2.0.0" {
		t.Errorf("Ident() = %q, want %q", unresolved.Ident(), "C-C-2.0.0")
	}
}

func TestResolveUnresolvedDependencyNamesTheMissingIdent(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"A-A@1.0.0": {"Missing-Package-9.9.9"},
	}}

	_, err := Resolve([]Request{{FullName: "A-A", Version: "1.0.0"}}, idx)
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v, want *UnresolvedError", err)
	}
	if unresolved.Ident() != "Missing-Package-9.9.9" {
		t.Errorf("Ident() = %q, want %q", unresolved.Ident(), "Missing-Package-9.9.9")
	}
}

// TestResolveHonoursTheRequestedVersion asserts a requested version is exact: the installed
// version is a no-op, a higher one an upgrade, and a lower one a downgrade that walks the lower
// version's own dependencies.
func TestResolveHonoursTheRequestedVersion(t *testing.T) {
	idx := &fakeIndex{
		deps: map[string][]string{
			"A-A@1.0.0": {"D-D-1.0.0"},
			"A-A@1.5.0": {},
			"A-A@2.0.0": {},
			"D-D@1.0.0": {},
		},
		installed: map[string]string{"A-A": "1.5.0"},
	}
	for _, tc := range []struct {
		request string
		want    Node
	}{
		{request: "1.5.0", want: Node{FullName: "A-A", Version: "1.5.0", NoOp: true}},
		{request: "2.0.0", want: Node{FullName: "A-A", Version: "2.0.0"}},
		{request: "1.0.0", want: Node{FullName: "A-A", Version: "1.0.0"}},
	} {
		closure, err := Resolve([]Request{{FullName: "A-A", Version: tc.request}}, idx)
		if err != nil {
			t.Fatal(err)
		}
		if got := closure.Nodes[0]; got != tc.want {
			t.Errorf("request %s = %+v, want %+v", tc.request, got, tc.want)
		}
		if tc.request == "1.0.0" {
			if d := nodeByName(t, closure.Nodes, "D-D"); !d.Transitive || d.Version != "1.0.0" {
				t.Errorf("D-D = %+v, want the downgrade's own dependency", d)
			}
		}
	}
}

// TestResolveNeverMovesAHeldPackage asserts a held package keeps its installed version: a
// request for another version is a HeldError, and a dependency edge above it is left for Check
// to report rather than raised.
func TestResolveNeverMovesAHeldPackage(t *testing.T) {
	idx := &fakeIndex{
		deps: map[string][]string{
			"A-A@1.0.0": {"L-L-2.0.0"},
			"L-L@1.0.0": {},
			"L-L@2.0.0": {},
		},
		installed: map[string]string{"L-L": "1.0.0"},
		held:      map[string]bool{"L-L": true},
	}

	_, err := Resolve([]Request{{FullName: "L-L", Version: "2.0.0"}}, idx)
	var held *HeldError
	if !errors.As(err, &held) || held.Version != "1.0.0" {
		t.Fatalf("err = %v, want a HeldError naming 1.0.0", err)
	}

	closure, err := Resolve([]Request{{FullName: "A-A", Version: "1.0.0"}}, idx)
	if err != nil {
		t.Fatal(err)
	}
	if l := nodeByName(t, closure.Nodes, "L-L"); l.Version != "1.0.0" || !l.NoOp {
		t.Errorf("L-L = %+v, want it held at 1.0.0", l)
	}
	planned := map[string]string{"A-A": "1.0.0", "L-L": "1.0.0"}
	want := []Conflict{{FullName: "A-A", Version: "1.0.0", Dependency: "L-L", Requires: "2.0.0", Have: "1.0.0"}}
	if got := Check(planned, map[string]bool{"A-A": true}, idx); !slices.Equal(got, want) {
		t.Errorf("Check = %+v, want %+v", got, want)
	}
}

// TestResolveKeepsARequestedVersionAgainstAHigherEdge asserts a diamond does not raise a
// requested version: the edge above it is Check's to report.
func TestResolveKeepsARequestedVersionAgainstAHigherEdge(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"A-A@1.0.0": {"D-D-2.0.0"},
		"D-D@1.0.0": {},
		"D-D@2.0.0": {},
	}}
	closure, err := Resolve([]Request{
		{FullName: "A-A", Version: "1.0.0"},
		{FullName: "D-D", Version: "1.0.0"},
	}, idx)
	if err != nil {
		t.Fatal(err)
	}
	if d := nodeByName(t, closure.Nodes, "D-D"); d.Version != "1.0.0" {
		t.Errorf("D-D = %+v, want the requested 1.0.0", d)
	}
}

// TestCheckReportsOnlyEdgesAChangeTouches asserts a downgrade breaking an installed dependent is
// reported, as is a removed dependency, while an edge that was already unmet and that the change
// does not touch is not.
func TestCheckReportsOnlyEdgesAChangeTouches(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"Y-Y@1.0.0": {"D-D-1.5.0"},
		"Z-Z@1.0.0": {"R-R-1.0.0"},
		"O-O@1.0.0": {"M-M-1.0.0"},
		"D-D@1.0.0": {},
	}}
	planned := map[string]string{"Y-Y": "1.0.0", "Z-Z": "1.0.0", "O-O": "1.0.0", "D-D": "1.0.0"}
	got := Check(planned, map[string]bool{"D-D": true, "R-R": true}, idx)
	want := []Conflict{
		{FullName: "Y-Y", Version: "1.0.0", Dependency: "D-D", Requires: "1.5.0", Have: "1.0.0"},
		{FullName: "Z-Z", Version: "1.0.0", Dependency: "R-R", Requires: "1.0.0"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Check = %+v, want %+v", got, want)
	}
}

// TestResolveWalksTheHigherVersionsOwnDependencies guards the expanded gate's key. D is
// reached at 1.0.0 through A and at 2.0.0 through B, and only 2.0.0 depends on E. Keyed by
// package rather than by package-and-version, the gate would report D at 2.0.0 while
// keeping 1.0.0's dependency list — handing the installer a closure with E missing.
func TestResolveWalksTheHigherVersionsOwnDependencies(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"R-Root@1.0.0": {"A-A-1.0.0", "B-B-1.0.0"},
		"A-A@1.0.0":    {"D-D-1.0.0"},
		"B-B@1.0.0":    {"D-D-2.0.0"},
		"D-D@1.0.0":    {},
		"D-D@2.0.0":    {"E-E-1.0.0"},
		"E-E@1.0.0":    {},
	}}

	closure, err := Resolve([]Request{{FullName: "R-Root", Version: "1.0.0"}}, idx)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeByName(t, closure.Nodes, "D-D"); got.Version != "2.0.0" {
		t.Errorf("D-D = %s, want 2.0.0", got.Version)
	}
	if got := nodeByName(t, closure.Nodes, "E-E"); got.Version != "1.0.0" {
		t.Errorf("E-E = %+v, want it present at 1.0.0", got)
	}
}

// TestResolveStopsAtAnAlreadySatisfiedNode: a package already installed at or above the
// requested version changes nothing, so it contributes no edges — its own dependencies
// came in with it, and the version it was installed from need not still be in the index
// after a re-sync.
func TestResolveStopsAtAnAlreadySatisfiedNode(t *testing.T) {
	idx := &fakeIndex{
		deps: map[string][]string{
			"A-A@1.0.0": {"D-D-1.0.0"},
			// D-D@1.5.0 is deliberately absent: it is installed, not indexed.
			"D-D@1.0.0": {"GONE-GONE-9.9.9"},
		},
		installed: map[string]string{"D-D": "1.5.0"},
	}

	closure, err := Resolve([]Request{{FullName: "A-A", Version: "1.0.0"}}, idx)
	if err != nil {
		t.Fatal(err)
	}
	d := nodeByName(t, closure.Nodes, "D-D")
	if d.Version != "1.5.0" || !d.NoOp {
		t.Errorf("D-D = %+v, want the installed 1.5.0 as a no-op", d)
	}
	for _, n := range closure.Nodes {
		if n.FullName == "GONE-GONE" {
			t.Error("a satisfied node's dependencies must not be walked")
		}
	}
}

func TestResolveMalformedDependencyIsTypedNotGeneric(t *testing.T) {
	idx := &fakeIndex{deps: map[string][]string{
		"A-A@1.0.0": {"Weird-Mod-1.0"},
	}}

	_, err := Resolve([]Request{{FullName: "A-A", Version: "1.0.0"}}, idx)
	var malformed *MalformedDependencyError
	if !errors.As(err, &malformed) {
		t.Fatalf("err = %v, want *MalformedDependencyError", err)
	}
	if malformed.Ident() != "Weird-Mod-1.0" {
		t.Errorf("Ident() = %q, want %q", malformed.Ident(), "Weird-Mod-1.0")
	}
}

// TestResolveRefusesAnUnusableVersion: an installed row or a request carrying something
// that is not major.minor.patch is reported, never silently treated as "nothing to do".
func TestResolveRefusesAnUnusableVersion(t *testing.T) {
	idx := &fakeIndex{
		deps:      map[string][]string{"A-A@2.0.0": {}},
		installed: map[string]string{"A-A": "1.0"}, // not strict major.minor.patch
	}

	_, err := Resolve([]Request{{FullName: "A-A", Version: "2.0.0"}}, idx)
	var bad *BadVersionError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want *BadVersionError for the installed 1.0", err)
	}

	_, err = Resolve([]Request{{FullName: "A-A", Version: "not-a-version"}}, &fakeIndex{})
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want *BadVersionError for the requested version", err)
	}
}

func TestParseDependencySplitsOnTheTrailingVersion(t *testing.T) {
	fullName, version, ok := ParseDependency("denikson-BepInExPack_Valheim-5.4.2333")
	if !ok || fullName != "denikson-BepInExPack_Valheim" || version != "5.4.2333" {
		t.Errorf("got %q, %q, %v", fullName, version, ok)
	}
}

func TestParseDependencyRejectsMalformed(t *testing.T) {
	for _, ident := range []string{"", "no-version-here", "Name-1.2", "Name-1.2.3.4"} {
		if _, _, ok := ParseDependency(ident); ok {
			t.Errorf("ParseDependency(%q) ok = true, want false", ident)
		}
	}
}

// TestParseDependencyAcceptsAPreReleasePin covers the 149 pins measured on a live registry
// that name a pre-release. Rejecting them made every package carrying one uninstallable,
// with dependency_unresolved as the only explanation.
func TestParseDependencyAcceptsAPreReleasePin(t *testing.T) {
	tests := []struct {
		ident    string
		fullName string
		version  string
		ok       bool
	}{
		{"ArgusMagnus-ServersideQoL-2.0.14-beta.5", "ArgusMagnus-ServersideQoL", "2.0.14-beta.5", true},
		{"LVH-IT-UseEquipmentInWater-0.2.4", "LVH-IT-UseEquipmentInWater", "0.2.4", true},
		{"bruceirons-team-PlantEverything_TEMP-1.20.1", "bruceirons-team-PlantEverything_TEMP", "1.20.1", true},
		{"Ns-Name-1.0.0-rc.1", "Ns-Name", "1.0.0-rc.1", true},
		{"Ns-Name-1.0", "", "", false},
		{"Ns-Name-1.0.0-", "", "", false},
		{"NoVersionHere", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.ident, func(t *testing.T) {
			fullName, version, ok := ParseDependency(tt.ident)
			if ok != tt.ok || fullName != tt.fullName || version != tt.version {
				t.Fatalf("ParseDependency(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.ident, fullName, version, ok, tt.fullName, tt.version, tt.ok)
			}
		})
	}
}

// TestDiamondOrdersAPreReleaseBelowItsCore: a pre-release loses to the same core release and
// beats the release below it, so neither edge of a diamond can downgrade the other.
func TestDiamondOrdersAPreReleaseBelowItsCore(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     string
	}{
		{"stable beats its own release candidate", []string{"1.1.0-rc.1", "1.1.0"}, "1.1.0"},
		{"and the order of the edges does not matter", []string{"1.1.0", "1.1.0-rc.1"}, "1.1.0"},
		{"a release candidate still raises an older release", []string{"1.0.0", "1.1.0-rc.1"}, "1.1.0-rc.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := fakeIndex{deps: map[string][]string{}}
			var requests []Request
			for i, v := range tt.versions {
				root := fmt.Sprintf("Ns-R%d", i)
				idx.deps[root+"@1.0.0"] = []string{"Ns-C-" + v}
				idx.deps["Ns-C@"+v] = nil
				requests = append(requests, Request{FullName: root, Version: "1.0.0"})
			}
			closure, err := Resolve(requests, &idx)
			if err != nil {
				t.Fatal(err)
			}
			if got := nodeByName(t, closure.Nodes, "Ns-C"); got.Version != tt.want {
				t.Errorf("resolved %q, want %q", got.Version, tt.want)
			}
		})
	}
}
