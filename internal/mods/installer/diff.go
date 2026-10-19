package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Action is what applying one Placement would do to the server root.
type Action string

const (
	ActionCreate    Action = "create"
	ActionOverwrite Action = "overwrite"
	ActionSkip      Action = "skip"
)

// Change is one Placement plus the verdict shown in the pre-apply diff (02 §4.2).
type Change struct {
	Placement
	Action Action
	// Reason is set only for ActionSkip. 03 §6.4 requires a shipped default that is not
	// written to be a line in the diff rather than a silence.
	Reason string
	// Unowned marks an overwrite of a file no installed package owns, such as one the
	// operator placed by hand. Writing it hands the file to this package, whose uninstall
	// would then delete it, so the operator decides (see Unowned).
	Unowned bool
}

// ConflictError is a destination another package's file manifest already owns —
// 11 §2.5's mod_conflict.
type ConflictError struct {
	Path  string
	Owner string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("installer: %s is already owned by %s", e.Path, e.Owner)
}

// Unowned is the operator's answer for files an install would overwrite that no installed
// package owns.
type Unowned string

const (
	// UnownedAsk refuses the install with an UnownedError listing the files.
	UnownedAsk Unowned = ""
	// UnownedKeep leaves the existing files and installs the package without them.
	UnownedKeep Unowned = "keep"
	// UnownedReplace overwrites the files and records them as the package's own.
	UnownedReplace Unowned = "replace"
)

// Valid reports whether u is one of the three answers.
func (u Unowned) Valid() bool { return u == UnownedAsk || u == UnownedKeep || u == UnownedReplace }

// UnownedError lists the files an install would overwrite that no installed package owns.
type UnownedError struct {
	Paths []string
}

func (e *UnownedError) Error() string {
	return fmt.Sprintf("these files are already on the server and belong to no installed mod: %s. "+
		"Install again choosing to keep them or to replace them", strings.Join(e.Paths, ", "))
}

const skipUnownedKept = "a file no mod owns was already there and is kept"

// ApplyUnowned applies the operator's answer to the unowned overwrites in changes, and returns the
// paths still waiting for one.
func ApplyUnowned(changes []Change, answer Unowned) []string {
	var waiting []string
	for i := range changes {
		c := &changes[i]
		if !c.Unowned {
			continue
		}
		switch answer {
		case UnownedKeep:
			c.Action, c.Reason = ActionSkip, skipUnownedKept
		case UnownedReplace:
		case UnownedAsk:
			waiting = append(waiting, c.Dest)
		}
	}
	return waiting
}

// configRoot is the one tree an install never overwrites: user settings live there
// (03 §6.4, B10).
const configRoot = "BepInEx/config/"

const skipConfigExists = "a config file already exists; user settings are never overwritten"

// UserConfig reports whether a destination lives in the tree an install never overwrites.
// Exported for the update and uninstall paths, which remove a package's other files but must
// leave these where they are.
func UserConfig(dest string) bool { return strings.HasPrefix(dest, configRoot) }

// Diff resolves each placement against the live server root and the paths other packages
// already own. claims maps a manifest path to the full name of the package that owns it, or
// to "" when its owner gives it up in the same change, so any package may take it.
//
// A placement landing on another package's path is a conflict, not an overwrite: the
// overwritten file would still be listed in that package's manifest, so its uninstall would
// later delete a file it no longer wrote (ADR-009).
func Diff(fullName string, placements []Placement, serverRoot string, claims map[string]string) ([]Change, error) {
	changes := make([]Change, 0, len(placements))
	for _, p := range placements {
		dest, err := CheckDest(p.Dest)
		if err != nil {
			return nil, err
		}
		exists, err := regularFileExists(filepath.Join(serverRoot, dest))
		if err != nil {
			return nil, err
		}
		if exists && strings.HasPrefix(p.Dest, configRoot) {
			changes = append(changes, Change{Placement: p, Action: ActionSkip, Reason: skipConfigExists})
			continue
		}
		if owner, ok := claims[p.Dest]; ok && owner != "" && owner != fullName {
			return nil, &ConflictError{Path: p.Dest, Owner: owner}
		}
		action := ActionCreate
		if exists {
			action = ActionOverwrite
		}
		_, claimed := claims[p.Dest]
		changes = append(changes, Change{Placement: p, Action: action, Unowned: exists && !claimed})
	}
	return changes, nil
}

// ErrUnsafeDest is a placement whose destination is not a relative path inside the server root
// (B5). Plan already refuses a full name that would produce one, but Diff is the step that turns
// a destination into a filesystem path, so it is the one that has to be sure.
var ErrUnsafeDest = errors.New("installer: destination escapes the server root")

// CheckDest validates dest and returns it as a cleaned, host-separator path for the caller to
// join. filepath.IsLocal is the containment test: it refuses anything absolute, rooted,
// drive-relative or climbing out, which scanning for "../" alone does not cover.
func CheckDest(dest string) (string, error) {
	cleaned := filepath.FromSlash(path.Clean(dest))
	if dest == "" || path.IsAbs(dest) || !filepath.IsLocal(cleaned) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeDest, dest)
	}
	return cleaned, nil
}

// ManifestEntry is one line of 04 §2's file_manifest: a path relative to the server root
// and the sha256 of the bytes written there.
type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// Parked records that disabling the package moved this file out of the server root into
	// the instance's parking tree, at the same relative path (Q37). It is recorded per entry,
	// not derived from the package's enabled flag, so every consumer knows exactly where each
	// file is, including one that was already missing when the package was disabled.
	Parked bool `json:"parked,omitempty"`
}

// Manifest is what uninstall and rollback read (ADR-009). A skipped change is deliberately
// absent: the file was already there, so it is not this package's to remove.
//
// The hash is taken from the staged source, computed before anything moves: the manifest is
// written first (12 §9.4), so a hash derived from the destination could only exist after the
// write it is meant to make reversible.
func Manifest(changes []Change) ([]ManifestEntry, error) {
	out := make([]ManifestEntry, 0, len(changes))
	for _, c := range changes {
		if c.Action == ActionSkip {
			continue
		}
		sum, err := sha256File(c.Source)
		if err != nil {
			return nil, err
		}
		out = append(out, ManifestEntry{Path: c.Dest, SHA256: sum})
	}
	return out, nil
}

// regularFileExists reports whether dest is an existing regular file. A destination that
// exists as a directory is neither a create nor an overwrite — no copy can succeed onto
// it — so it is reported rather than folded into either verdict.
func regularFileExists(dest string) (bool, error) {
	info, err := os.Lstat(dest)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("stat %s: %w", dest, err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%s exists and is not a regular file", dest)
	}
	return true, nil
}

func sha256File(src string) (string, error) {
	f, err := os.Open(src) //nolint:gosec // src comes from Plan, which only ever walks the staging directory
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", src, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
