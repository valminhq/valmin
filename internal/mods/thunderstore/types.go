package thunderstore

import "github.com/valminhq/valmin/internal/mods/semver"

// Package is one community package from the v1 listing. Fields the panel never reads are left
// undecoded, which encoding/json ignores silently.
//
// This shape is measured against a real response (testdata/v1-package-capture.json), not
// inferred from the OpenAPI spec, which declares "versions" as a bare string with no
// "dependencies" field, a drf-yasg artefact rather than the real shape documented in
// 03 §6.3.
type Package struct {
	Name         string    `json:"name"`
	FullName     string    `json:"full_name"`
	Owner        string    `json:"owner"`
	RatingScore  int       `json:"rating_score"`
	IsDeprecated bool      `json:"is_deprecated"`
	Categories   []string  `json:"categories"`
	Versions     []Version `json:"versions"`
}

// Version is one entry of Package.Versions.
type Version struct {
	Description   string   `json:"description"`
	Icon          string   `json:"icon"`
	VersionNumber string   `json:"version_number"`
	Dependencies  []string `json:"dependencies"`
	DownloadURL   string   `json:"download_url"`
	Downloads     int64    `json:"downloads"`
	FileSize      int64    `json:"file_size"`
}

// Latest returns the version with the highest version_number, compared as strict
// major.minor.patch (03 §6.2), never simply Versions[0]: the listing's newest-first order is
// observed, not documented (E8).
func (p *Package) Latest() (Version, bool) {
	var best Version
	var bestParsed [3]int
	found := false
	for _, v := range p.Versions {
		parsed, ok := semver.Parse(v.VersionNumber)
		if !ok {
			continue
		}
		if !found || semver.Greater(parsed, bestParsed) {
			best, bestParsed, found = v, parsed, true
		}
	}
	if found {
		return best, true
	}
	// Every version_number failed to parse: fall back to the listing's own order rather
	// than reporting no versions for a package that plainly has some.
	if len(p.Versions) > 0 {
		return p.Versions[0], true
	}
	return Version{}, false
}

// TotalDownloads sums Downloads across every version — the aggregate Thunderstore's own
// site shows, since the v1 listing carries no package-level total of its own.
func (p *Package) TotalDownloads() int64 {
	var total int64
	for _, v := range p.Versions {
		total += v.Downloads
	}
	return total
}
