package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// TestTheBuildIsIdentifiedWithoutLinkerFlags asserts the version control stamps the go tool
// embeds are enough to name a build, so an unreleased binary is still identifiable.
func TestTheBuildIsIdentifiedWithoutLinkerFlags(t *testing.T) {
	got := fromBuildInfo(&debug.BuildInfo{
		GoVersion: "go1.25.0",
		Main:      debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "d6ebba7d278b3b6a03542970b96a857a67f10037"},
			{Key: "vcs.time", Value: "2026-09-12T09:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	want := Build{
		Version: "(devel)", Commit: "d6ebba7d278b3b6a03542970b96a857a67f10037",
		BuiltAt: "2026-09-12T09:00:00Z", Modified: true, Go: "go1.25.0",
	}
	if got != want {
		t.Fatalf("fromBuildInfo = %+v, want %+v", got, want)
	}
	if line := got.String(); !strings.Contains(line, "modified") {
		t.Errorf("version line %q does not say the tree was modified", line)
	}
}

// TestTheReleaseIdentityWins asserts a linker-set version outranks the recorded module
// version: a release artefact names its tag.
func TestTheReleaseIdentityWins(t *testing.T) {
	t.Cleanup(func() { Version = "" })
	Version = "v1.0.0"

	got := fromBuildInfo(&debug.BuildInfo{GoVersion: "go1.25.0", Main: debug.Module{Version: "(devel)"}})
	if got.Version != "v1.0.0" {
		t.Errorf("version = %q, want the link-time v1.0.0", got.Version)
	}
}

// TestALinkTimeCommitSurvivesAnUnstampedBuild asserts an image build, which has no
// repository to read, still names the revision it was built from.
func TestALinkTimeCommitSurvivesAnUnstampedBuild(t *testing.T) {
	t.Cleanup(func() { Commit = "" })
	Commit = "df381f9dda409de44bad8c1d37f1a49f49361d0f"

	got := fromBuildInfo(&debug.BuildInfo{GoVersion: "go1.25.0", Main: debug.Module{Version: "(devel)"}})
	if got.Commit != Commit {
		t.Errorf("commit = %q, want the link-time %q", got.Commit, Commit)
	}
}
