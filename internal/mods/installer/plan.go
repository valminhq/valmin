package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Placement is one staged file and the path it will occupy under the instance's server
// root.
type Placement struct {
	// Source is the absolute path of the file inside the staging directory.
	Source string
	// Dest is slash-separated and relative to the server root — the form 04 §2's
	// file_manifest records.
	Dest string
}

// ErrUnsupportedEntry is a staged entry that is neither a regular file nor a directory.
// internal/mods/extract already refuses symlinks, so this only fires on a staging
// directory that something other than Extract produced.
var ErrUnsupportedEntry = errors.New("installer: unsupported staged entry")

// ErrInvalidFullName is a package full name that is not usable as a single path segment.
//
// `↯` B5. full_name arrives from the Thunderstore listing and is interpolated into the
// namespaced plugin directory, and path.Join resolves `..` as it builds — so a package
// named "../../../../etc/cron.d" yields destinations outside the server root, which the
// manifest then records and WP-07 writes. Nothing upstream constrains it: the sync only
// requires it to be non-empty, and resolver.ParseDependency's pattern accepts slashes and
// dots. This is the boundary that has to refuse it.
var ErrInvalidFullName = errors.New("installer: invalid package full name")

// DuplicateDestError is two staged files claiming one destination inside a single package.
// Per-entry classification (ADR-106) makes it reachable — a package shipping both
// plugins/Shared.dll and BepInEx/plugins/Shared.dll produces one destination twice — and
// the manifest would then carry that path twice with two different hashes, only one of
// which could ever match disk. That breaks ADR-009's exact uninstall, so it is refused
// rather than resolved by letting one file win.
type DuplicateDestError struct {
	Dest   string
	First  string
	Second string
}

func (e *DuplicateDestError) Error() string {
	return fmt.Sprintf("installer: %s and %s both place %s", e.First, e.Second, e.Dest)
}

// metadata is 03 §6.4's exclusion list, matched case-insensitively — the four names are a
// packaging convention rather than a format rule, and the corpus's own capitalisation of
// README.md and CHANGELOG.md is not guaranteed by anything.
var metadata = map[string]bool{
	"manifest.json": true,
	"icon.png":      true,
	"readme.md":     true,
	"changelog.md":  true,
}

// mergeDirs are the top-level directory names 03 §6.4 merges into the shared BepInEx tree
// rather than namespacing under the package.
var mergeDirs = map[string]string{
	"BepInEx":  "BepInEx",
	"plugins":  "BepInEx/plugins",
	"config":   "BepInEx/config",
	"patchers": "BepInEx/patchers",
}

// Plan lists every file a staged package would place under the server root, in a
// deterministic order. It reads the staging directory and nothing else: whether a
// destination already exists is Diff's question, not this one's.
//
// `↯` Classification is per top-level entry, not per package (ADR-106). 03 §6.4's
// original "apply in order" wording stops at the first heuristic that matches, and three
// packages in the corpus — Therzie-Warfare, -Monstrum and -Armory — ship a root .dll
// *and* a top-level config/, so heuristic 3 matched and the .dll was never placed at all.
// The single-wrapper case below is the one that stays whole-package, because merging half
// a framework pack into the server root is not a thing that can be done per entry.
func Plan(stagingDir, fullName string) ([]Placement, error) {
	if err := CheckFullName(fullName); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve staging directory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read staging directory %s: %w", root, err)
	}

	payload := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !metadata[strings.ToLower(e.Name())] {
			payload = append(payload, e)
		}
	}

	// One WalkDir over one tree cannot yield the same relative path twice, so the wrapper
	// branch needs no duplicate check — unlike the per-entry branch, where two top-level
	// entries can classify onto one destination.
	if wrapper, ok := frameworkWrapper(root, payload); ok {
		return walkFiles(filepath.Join(root, wrapper), "")
	}

	var out []Placement
	for _, e := range payload {
		src := filepath.Join(root, e.Name())
		dest := destFor(e, fullName)
		if !e.IsDir() {
			if !e.Type().IsRegular() {
				return nil, fmt.Errorf("%w: %s", ErrUnsupportedEntry, e.Name())
			}
			out = append(out, Placement{Source: src, Dest: dest})
			continue
		}
		sub, err := walkFiles(src, dest)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	if err := checkNoDuplicateDest(out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckFullName refuses anything that is not a single path segment: 03 §6.2's full name is
// "Namespace-Name", and a separator or a dot-segment in it is a traversal rather than a
// package. Exported because Plan is not the first thing to use a full name as a path — the
// job stages each package into a directory named after it, and that happens earlier.
func CheckFullName(fullName string) error {
	switch {
	case fullName == "":
		return fmt.Errorf("%w: empty", ErrInvalidFullName)
	case strings.ContainsAny(fullName, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidFullName, fullName)
	case fullName == "." || fullName == "..":
		return fmt.Errorf("%w: %q", ErrInvalidFullName, fullName)
	}
	return nil
}

func checkNoDuplicateDest(placements []Placement) error {
	seen := make(map[string]string, len(placements))
	for _, p := range placements {
		if first, ok := seen[p.Dest]; ok {
			return &DuplicateDestError{Dest: p.Dest, First: first, Second: p.Source}
		}
		seen[p.Dest] = p.Source
	}
	return nil
}

// destFor is the destination of one top-level entry. Only directories are matched against
// mergeDirs: a loose file named "config" is a file, and merging it onto the BepInEx/config
// directory path would be a collision rather than a placement.
func destFor(e fs.DirEntry, fullName string) string {
	if e.IsDir() {
		if dest, ok := mergeDirs[e.Name()]; ok {
			return dest
		}
	}
	return path.Join("BepInEx/plugins", fullName, e.Name())
}

// frameworkWrapper reports the single top-level directory a framework package wraps its
// entire server-root tree in — 03 §6.4's heuristic 1, the denikson-BepInExPack_Valheim
// shape.
//
// `↯` A BepInEx/ child is required, not just "one top-level directory". A plugin that
// happens to ship its files inside one folder is not a framework pack, and merging it into
// the server root would scatter a mod's DLLs beside the game binary with no manifest entry
// pointing at where they went.
func frameworkWrapper(root string, payload []fs.DirEntry) (string, bool) {
	if len(payload) != 1 || !payload[0].IsDir() {
		return "", false
	}
	name := payload[0].Name()
	info, err := os.Stat(filepath.Join(root, name, "BepInEx"))
	if err != nil || !info.IsDir() {
		return "", false
	}
	return name, true
}

// walkFiles collects every regular file under dir, rooted at destPrefix. Directories are
// not placements: the applier creates each destination's parents, and an empty directory
// the archive happened to carry deploys nothing and records nothing in the manifest.
// filepath.WalkDir visits lexically, which is what makes Plan's result deterministic.
func walkFiles(dir, destPrefix string) ([]Placement, error) {
	var out []Placement
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrUnsupportedEntry, p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return fmt.Errorf("relativise %s: %w", p, err)
		}
		out = append(out, Placement{Source: p, Dest: path.Join(destPrefix, filepath.ToSlash(rel))})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	return out, nil
}
