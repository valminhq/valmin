// Package version reports which build of the panel is running. An operator quoting a bug
// report, and the release artefacts of a candidate, name the same build through it.
package version

import (
	"runtime/debug"
	"sync"
)

// Version is the release identity, set at link time by the release build:
//
//	-X github.com/valminhq/valmin/internal/version.Version=v1.0.0
//
// It is empty for every other build, which then reports what the go tool recorded.
var Version string

// Commit is the revision, set at link time the same way. It exists for builds the go tool
// cannot stamp itself: an image build has no .git to read (the release identity is a build
// argument), and a build from a source tarball has no repository at all.
var Commit string

// Build is one build's identity. Commit and BuiltAt come from the version control stamps
// the go tool embeds, so a plain `go build` of a checkout is already identified and a
// release only adds the tag.
type Build struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	BuiltAt  string `json:"built_at"`
	Modified bool   `json:"modified"`
	Go       string `json:"go"`
}

// String is the one line `valmind version` prints and the startup log carries.
func (b Build) String() string {
	s := b.Version
	if b.Commit != "" {
		s += " (" + b.Commit
		if b.Modified {
			s += ", modified"
		}
		s += ")"
	}
	if b.BuiltAt != "" {
		s += " built " + b.BuiltAt
	}
	return s + " " + b.Go
}

// Current reports this build. It is read once: the answer cannot change while the process
// runs, and ReadBuildInfo walks the whole embedded module graph.
var Current = sync.OnceValue(func() Build {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Build{Version: version(""), Commit: Commit, Go: "unknown"}
	}
	return fromBuildInfo(info)
})

func fromBuildInfo(info *debug.BuildInfo) Build {
	b := Build{Version: version(info.Main.Version), Commit: Commit, Go: info.GoVersion}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				b.Commit = s.Value
			}
		case "vcs.time":
			b.BuiltAt = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// version prefers the link-time identity. "(devel)" is what the go tool records for a build
// from a checkout rather than a module release, and it is reported as such: a build claiming
// a version it does not have is worse than one admitting it has none.
func version(recorded string) string {
	switch {
	case Version != "":
		return Version
	case recorded != "":
		return recorded
	default:
		return "(devel)"
	}
}
