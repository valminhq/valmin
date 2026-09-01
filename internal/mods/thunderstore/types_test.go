package thunderstore

import "testing"

// TestPackageLatestPicksHighestSemverNotArrayOrder is why Latest() parses rather than
// trusts position: "1.10.0" sorts before "1.2.0" and "1.9.9" as a plain string, but is
// numerically the highest of the three.
func TestPackageLatestPicksHighestSemverNotArrayOrder(t *testing.T) {
	p := Package{Versions: []Version{
		{VersionNumber: "1.2.0"},
		{VersionNumber: "1.10.0"},
		{VersionNumber: "1.9.9"},
	}}
	got, ok := p.Latest()
	if !ok {
		t.Fatal("Latest() ok = false")
	}
	if got.VersionNumber != "1.10.0" {
		t.Errorf("Latest() = %q, want %q — string order would wrongly prefer 1.9.9 or 1.2.0",
			got.VersionNumber, "1.10.0")
	}
}

func TestPackageLatestFallsBackWhenNothingParses(t *testing.T) {
	p := Package{Versions: []Version{{VersionNumber: "not-a-version"}}}
	got, ok := p.Latest()
	if !ok || got.VersionNumber != "not-a-version" {
		t.Errorf("Latest() = %+v, %v; want the sole entry as a fallback", got, ok)
	}
}

func TestPackageLatestOfEmptyPackage(t *testing.T) {
	p := Package{}
	if _, ok := p.Latest(); ok {
		t.Error("Latest() of a package with no versions = ok, want false")
	}
}

func TestPackageTotalDownloadsSumsEveryVersion(t *testing.T) {
	p := Package{Versions: []Version{
		{Downloads: 100},
		{Downloads: 250},
		{Downloads: 7},
	}}
	if got := p.TotalDownloads(); got != 357 {
		t.Errorf("TotalDownloads() = %d, want 357", got)
	}
}
