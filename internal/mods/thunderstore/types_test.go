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

// TestLatestNeverChoosesAPreRelease: Latest decides mod_packages.latest_version, which is
// what the install button offers and what the framework pin resolves to. A pre-release is
// installable when something pins it and never when the panel is the one choosing.
func TestLatestNeverChoosesAPreRelease(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     string
	}{
		{"a pre-release above the newest stable", []string{"2.1.0-beta.1", "2.0.0"}, "2.0.0"},
		{"listed the other way round", []string{"2.0.0", "2.1.0-beta.1"}, "2.0.0"},
		{"stable only", []string{"1.0.0", "2.0.0", "1.5.0"}, "2.0.0"},
		{"nothing but pre-releases", []string{"1.0.0-rc.1", "1.0.0-beta.2"}, "1.0.0-rc.1"},
		{"a pre-release of the same core as the stable", []string{"2.0.0-rc.1", "2.0.0"}, "2.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Package{FullName: "Ns-Name"}
			for _, v := range tt.versions {
				p.Versions = append(p.Versions, Version{VersionNumber: v})
			}
			got, ok := p.Latest()
			if !ok {
				t.Fatal("Latest reported no versions")
			}
			if got.VersionNumber != tt.want {
				t.Errorf("Latest() = %q, want %q", got.VersionNumber, tt.want)
			}
		})
	}
}
