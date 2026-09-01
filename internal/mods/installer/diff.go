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

// configRoot is the one tree an install never overwrites: user settings live there
// (03 §6.4, B10).
const configRoot = "BepInEx/config/"

const skipConfigExists = "a config file already exists; user settings are never overwritten"

// Diff resolves each placement against the live server root and the paths other packages
// already own. claims maps a manifest path to the full name of the package that owns it.
//
// `↯` A placement landing on another package's path is a conflict, not an overwrite: the
// overwritten file would still be listed in the other package's manifest, so that
// package's uninstall would later delete a file it no longer wrote — the orphan-DLL
// failure ADR-009 exists to prevent, inverted.
func Diff(fullName string, placements []Placement, serverRoot string, claims map[string]string) ([]Change, error) {
	changes := make([]Change, 0, len(placements))
	for _, p := range placements {
		if err := checkDest(p.Dest); err != nil {
			return nil, err
		}
		exists, err := regularFileExists(filepath.Join(serverRoot, filepath.FromSlash(p.Dest)))
		if err != nil {
			return nil, err
		}
		if exists && strings.HasPrefix(p.Dest, configRoot) {
			changes = append(changes, Change{Placement: p, Action: ActionSkip, Reason: skipConfigExists})
			continue
		}
		if owner, ok := claims[p.Dest]; ok && owner != fullName {
			return nil, &ConflictError{Path: p.Dest, Owner: owner}
		}
		action := ActionCreate
		if exists {
			action = ActionOverwrite
		}
		changes = append(changes, Change{Placement: p, Action: action})
	}
	return changes, nil
}

// ErrUnsafeDest is a placement whose destination is not a relative path inside the server
// root (B5). Plan cannot produce one — it refuses a full name that would — but Diff is a
// separate entry point, and it is the step that turns a destination into a filesystem path
// and hands it to the applier, so it is the one that has to be sure.
var ErrUnsafeDest = errors.New("installer: destination escapes the server root")

func checkDest(dest string) error {
	cleaned := path.Clean(dest)
	if dest == "" || path.IsAbs(dest) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%w: %q", ErrUnsafeDest, dest)
	}
	return nil
}

// ManifestEntry is one line of 04 §2's file_manifest: a path relative to the server root
// and the sha256 of the bytes written there.
type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is what uninstall and rollback read (ADR-009). A skipped change is deliberately
// absent: uninstall must never remove a file this package did not write, which is what
// keeps a user-edited .cfg alive across an uninstall.
//
// The hash is taken from the staged source, whose bytes are what the applier copies. It is
// computed before anything moves, because 12 §9.4 fixes the manifest as the thing written
// first — a hash derived from the destination could only exist after the write it is meant
// to make reversible.
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
