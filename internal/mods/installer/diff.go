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
	// SHA256 is the hash of the staged bytes.
	SHA256 string
	// Kept marks a config file the package ships that is left as found on disk.
	Kept bool
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

// configRoot is the tree that holds user settings. An install writes an existing file there
// only when the package's own shipped version of it changed.
const configRoot = "BepInEx/config/"

const skipConfigExists = "a config file already there, or placed by another mod, is kept"

// UserConfig reports whether a destination lives in the config tree. The update and uninstall
// paths remove a package's other files but leave these where they are.
func UserConfig(dest string) bool { return strings.HasPrefix(dest, configRoot) }

// Diff resolves each placement against the live server root and the paths other packages
// already own. claims maps a manifest path to the full name of the package that owns it, or
// to "" when its owner gives it up in the same change, so any package may take it. prev maps
// each path of the version being replaced to the hash it shipped; it is empty for a fresh
// install or a repair.
//
// A config file that exists, or that another package places, is decided by configChange. Any
// other placement landing on another package's path is a conflict, not an overwrite: the file
// would still be listed in that package's manifest, so its uninstall would later delete it.
func Diff(
	fullName string, placements []Placement, serverRoot string, claims, prev map[string]string,
) ([]Change, error) {
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
		sum, err := sha256File(p.Source)
		if err != nil {
			return nil, err
		}
		owner := claims[p.Dest]
		otherOwner := owner != "" && owner != fullName
		if UserConfig(p.Dest) && (exists || otherOwner) {
			c, err := configChange(p, sum, serverRoot, exists, prev)
			if err != nil {
				return nil, err
			}
			changes = append(changes, c)
			continue
		}
		if otherOwner {
			return nil, &ConflictError{Path: p.Dest, Owner: owner}
		}
		action := ActionCreate
		if exists {
			action = ActionOverwrite
		}
		_, claimed := claims[p.Dest]
		changes = append(changes, Change{Placement: p, Action: action, Unowned: exists && !claimed, SHA256: sum})
	}
	return changes, nil
}

// configChange decides a config file that exists or that another package places. It is
// overwritten when it exists, the package's previous version shipped different bytes, and the
// file does not already hold the new ones; otherwise it is kept as found.
func configChange(p Placement, sum, serverRoot string, exists bool, prev map[string]string) (Change, error) {
	if was, ok := prev[p.Dest]; ok && exists && was != sum {
		have, err := hashLive(serverRoot, p.Dest)
		if err != nil {
			return Change{}, err
		}
		if have != sum {
			return Change{Placement: p, Action: ActionOverwrite, SHA256: sum}, nil
		}
	}
	return Change{Placement: p, Action: ActionSkip, Reason: skipConfigExists, SHA256: sum, Kept: true}, nil
}

// hashLive hashes the file at dest inside serverRoot, reading through a root so a symlink
// planted in the server tree is not followed out of it.
func hashLive(serverRoot, dest string) (string, error) {
	rel, err := CheckDest(dest)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(serverRoot)
	if err != nil {
		return "", fmt.Errorf("open server root: %w", err)
	}
	defer func() { _ = root.Close() }()
	return hashIn(root, rel)
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
	// Kept marks a config file the package ships that the install left as found. SHA256 is the
	// shipped bytes, which the next update compares against; the file itself was never written,
	// so a rollback without a saved copy leaves it alone.
	Kept bool `json:"kept,omitempty"`
}

// Manifest is what uninstall and rollback read: every file the changes write, plus the config
// files the package ships that were kept as found. Any other skipped change is absent, since
// the file is not this package's. Hashes are of the staged bytes, so the manifest can be
// written before anything moves.
func Manifest(changes []Change) []ManifestEntry {
	out := make([]ManifestEntry, 0, len(changes))
	for _, c := range changes {
		if c.Action == ActionSkip && !c.Kept {
			continue
		}
		out = append(out, ManifestEntry{Path: c.Dest, SHA256: c.SHA256, Kept: c.Kept})
	}
	return out
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
	f, err := os.Open(src)
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
