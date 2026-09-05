// Package resolver computes a mod install's dependency closure — 03 §6.3's diamond
// resolution and cycle detection — as pure functions over caller-supplied lookups. It
// imports neither store nor api: the caller — the job runner — already
// holds the mod_versions rows and the instance's installed set, and passes them in rather
// than this package reading either directly.
package resolver

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/valminhq/valmin/internal/mods/semver"
)

// Request is one explicitly-requested install.
type Request struct {
	FullName string
	Version  string
}

// Node is one resolved package in the closure.
type Node struct {
	FullName string
	Version  string
	// Transitive is false only for a package named directly in Resolve's requests.
	Transitive bool
	// NoOp is true when an already-installed version satisfies this node, which is the case for
	// an install requesting a lower version than the one present. Version is then the installed
	// version, not the requested one.
	NoOp bool
}

// Closure is Resolve's result, in discovery order — stable across repeated calls with the
// same inputs, not meaningful beyond that.
type Closure struct {
	Nodes []Node
}

// Index is what Resolve needs, supplied by the caller so this package touches neither
// store nor the network (CLAUDE.md §5).
type Index interface {
	// Dependencies returns fullName-version's own dependency idents (03 §6.3's raw
	// strings, each "Namespace-Name-Version"), and whether that exact version is known to
	// the cached index at all.
	Dependencies(fullName, version string) (deps []string, ok bool)
	// Installed reports the version of fullName currently installed on the target
	// instance, or ok=false if it is not installed there.
	Installed(fullName string) (version string, ok bool)
}

// UnresolvedError is a dependency naming a package/version the index does not have —
// 11 §2.5's dependency_unresolved, details.missing.
type UnresolvedError struct {
	FullName string
	Version  string
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("resolver: %s-%s is not in the index", e.FullName, e.Version)
}

// Ident is the dependency string this error names, for a caller building details.missing.
func (e *UnresolvedError) Ident() string { return e.FullName + "-" + e.Version }

// MalformedDependencyError is a dependency ident that does not end in a strict
// major.minor.patch version. The index is externally sourced and does not always honour
// 03 §6.2's format, so this is unusable data rather than a panel fault, reported as an absent
// dependency is.
type MalformedDependencyError struct {
	FullName   string
	Version    string
	Dependency string
}

func (e *MalformedDependencyError) Error() string {
	return fmt.Sprintf("resolver: %s-%s names a malformed dependency %q", e.FullName, e.Version, e.Dependency)
}

// Ident is the offending dependency string, for a caller building details.missing.
func (e *MalformedDependencyError) Ident() string { return e.Dependency }

// BadVersionError is a version string that is not strict major.minor.patch, whether requested or
// read back from a row this panel wrote. It is reported rather than compared around: treating an
// uncomparable version as satisfied would answer "nothing to do" to a real request.
type BadVersionError struct {
	FullName string
	Version  string
}

func (e *BadVersionError) Error() string {
	return fmt.Sprintf("resolver: %s has an unusable version %q", e.FullName, e.Version)
}

// CycleError names every package in a dependency cycle, in the order the cycle was
// walked, so a cycle is named rather than recursed into.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return "resolver: dependency cycle: " + strings.Join(e.Cycle, " -> ")
}

// depPattern splits a dependency ident into its package and version halves. The format is
// "Namespace-Name-Version" (03 §6.2), but either name half may contain hyphens, so this anchors
// on the trailing digit.digit.digit shape rather than counting them.
var depPattern = regexp.MustCompile(`^(.+)-(\d+\.\d+\.\d+)$`)

// ParseDependency splits one dependency ident. ok is false for anything that does not end
// in a strict major.minor.patch version.
func ParseDependency(ident string) (fullName, version string, ok bool) {
	m := depPattern.FindStringSubmatch(ident)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Resolve computes the closure requests need — 03 §6.3: a diamond resolves to the highest
// version any edge in the closure requested, and the closure is returned before anything
// downloads so the caller can show which packages are transitive.
func Resolve(requests []Request, idx Index) (Closure, error) {
	s := &resolveState{
		idx:      idx,
		highest:  map[string]string{},
		explicit: map[string]bool{},
		expanded: map[string]bool{},
	}

	for _, req := range requests {
		// Every version reaching walk is verified strict major.minor.patch here or by
		// ParseDependency's own pattern, so the comparisons below never have to guess at
		// a malformed one.
		if _, ok := semver.Parse(req.Version); !ok {
			return Closure{}, &BadVersionError{FullName: req.FullName, Version: req.Version}
		}
		s.explicit[req.FullName] = true
		if err := s.walk(req.FullName, req.Version, nil); err != nil {
			return Closure{}, err
		}
	}
	return s.closure(), nil
}

// resolveState is one Resolve call's working set — a struct rather than closures so walk
// is a plain method instead of a function nested inside Resolve.
type resolveState struct {
	idx      Index
	highest  map[string]string // fullName -> highest version seen so far
	explicit map[string]bool   // fullName -> named directly in Resolve's requests
	expanded map[string]bool   // "fullName@version" -> that version's dependencies are walked
	order    []string          // discovery order, for a stable Closure
}

// walk visits one dependency edge.
//
// The index lookup runs before the expanded gate, so every edge's exact version is verified to
// exist: behind it, a diamond could raise a node to a version nothing had checked and fail later
// as a download rather than here as dependency_unresolved.
//
// The gate is keyed by version, not by package: keyed by package, a node reached again at a
// higher version would keep the first version's dependency list and yield an incomplete
// closure.
func (s *resolveState) walk(fullName, version string, path []string) error {
	if err := checkCycle(path, fullName); err != nil {
		return err
	}
	installed, satisfied, err := s.installedSatisfies(fullName, version)
	if err != nil {
		return err
	}
	if satisfied {
		// Already present at this version or higher: no download and no edges, since whatever it
		// depends on came in with it, and a re-sync may have dropped its version from the index.
		s.recordHighest(fullName, installed)
		return nil
	}

	deps, ok := s.idx.Dependencies(fullName, version)
	if !ok {
		return &UnresolvedError{FullName: fullName, Version: version}
	}
	s.recordHighest(fullName, version)

	key := fullName + "@" + version
	if s.expanded[key] {
		return nil
	}
	s.expanded[key] = true

	nextPath := appendPath(path, fullName)
	for _, dep := range deps {
		depFullName, depVersion, ok := ParseDependency(dep)
		if !ok {
			return &MalformedDependencyError{FullName: fullName, Version: version, Dependency: dep}
		}
		if err := s.walk(depFullName, depVersion, nextPath); err != nil {
			return err
		}
	}
	return nil
}

// installedSatisfies reports whether fullName is already present at version or higher, and at
// which version. An installed version that is not strict major.minor.patch is an error rather
// than a silent verdict.
func (s *resolveState) installedSatisfies(fullName, version string) (installed string, ok bool, err error) {
	installed, present := s.idx.Installed(fullName)
	if !present {
		return "", false, nil
	}
	if _, parseOK := semver.Parse(installed); !parseOK {
		return "", false, &BadVersionError{FullName: fullName, Version: installed}
	}
	return installed, !higher(version, installed), nil
}

// recordHighest tracks the highest version reached for fullName across the whole closure
// (03 §6.3's diamond resolution) and its first-seen discovery order.
func (s *resolveState) recordHighest(fullName, version string) {
	cur, seen := s.highest[fullName]
	if !seen {
		s.order = append(s.order, fullName)
		s.highest[fullName] = version
		return
	}
	if higher(version, cur) {
		s.highest[fullName] = version
	}
}

// closure builds the final result. A node whose resolved version is exactly what is already
// installed is a no-op, effectiveVersion having already substituted the installed version
// wherever it satisfies the request.
func (s *resolveState) closure() Closure {
	nodes := make([]Node, 0, len(s.order))
	for _, fn := range s.order {
		n := Node{FullName: fn, Version: s.highest[fn], Transitive: !s.explicit[fn]}
		if installed, ok := s.idx.Installed(fn); ok && installed == n.Version {
			n.NoOp = true
		}
		nodes = append(nodes, n)
	}
	return Closure{Nodes: nodes}
}

// higher reports whether a is a higher version than b. Both must already be known to
// parse — Resolve, ParseDependency and effectiveVersion between them guarantee it.
func higher(a, b string) bool {
	pa, aOK := semver.Parse(a)
	pb, bOK := semver.Parse(b)
	return aOK && bOK && semver.Greater(pa, pb)
}

func checkCycle(path []string, fullName string) error {
	for _, p := range path {
		if p == fullName {
			return &CycleError{Cycle: appendPath(path, fullName)}
		}
	}
	return nil
}

// appendPath returns a new slice — path plus fullName — that never shares a backing array
// with path, so two sibling calls in the same walk can never overwrite each other's view
// of the path.
func appendPath(path []string, fullName string) []string {
	next := make([]string, len(path)+1)
	copy(next, path)
	next[len(path)] = fullName
	return next
}
