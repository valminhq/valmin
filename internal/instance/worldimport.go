package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/backup"
)

// WorldsLocalDir is the subdirectory of worlds/ the game keeps saves in (03 §4). Uploads
// arrive from three different source layouts and are all normalised into this one.
const WorldsLocalDir = "worlds_local"

// minDBBytes is 03 §4.1 rule 5's sanity floor on a world database, sized to catch an empty or
// truncated upload rather than to police world size: the smallest measured real world was
// 998 KB.
const minDBBytes = 1024

// ImportRule names why an upload was refused, without any HTTP concern — internal/api maps
// each to 11 §2.4's field shape.
type ImportRule string

const (
	RulePairIncomplete   ImportRule = "world_pair_incomplete"
	RuleBasenameMismatch ImportRule = "basename_mismatch"
	RuleDBTooSmall       ImportRule = "db_too_small"
	RuleNotAWorldFile    ImportRule = "not_a_world_file"
	RuleBackupVariant    ImportRule = "backup_variant"
	RuleUnsafeName       ImportRule = "unsafe_name"
)

// ImportViolation is one broken rule, with a message carrying the detail a user needs to
// fix it themselves.
type ImportViolation struct {
	Rule   ImportRule
	Detail string
}

func (v ImportViolation) Error() string { return string(v.Rule) + ": " + v.Detail }

// backupVariant matches the engine's own rolling saves, rejected by 03 §4.1 rule 5 unless the
// user explicitly picks one, since a user uploading a whole save folder rarely means to restore
// a previous state. `.old` is matched separately, since `World.db.old` has already lost the
// `.db` extension the pair check keys on.
//
// Both spellings: pre-1.0 wrote `<name>_backup_auto-20260903073824`, and 1.0 writes
// `<name>_backup_auto-20260913-204651` as a directory — the dash is why one pattern cannot be
// the old one (evidence/world-format-1.0-2026-09-14.md).
var backupVariant = regexp.MustCompile(`_backup_auto-[\d-]+$`)

// StagedFile is one file of an uploaded world: where it is now, and the name it must take
// under worlds_local/ once installed.
type StagedFile struct {
	Path string
	// Name is relative to worlds_local/ and is what the audited write boundary is given, so
	// the root check runs over the name the panel chose rather than one an upload supplied.
	Name string
}

// UploadedWorld is one candidate world, already staged on disk, in either of 03 §4's layouts.
type UploadedWorld struct {
	// Basename is the world's own name as uploaded: the shared stem of a `.db`/`.fwl` pair,
	// or the name of a 1.0 world directory.
	Basename string
	// Directory is 1.0's layout: the world is a directory of `_main.<gen>.*` files and chunks
	// rather than two files (03 §4, ADR-179).
	Directory bool
	DBPath    string
	FWLPath   string
	// Files are the world's own staged files, relative to the staging directory.
	Files []string
	// Info is the parsed world header, valid only once ValidateImport has returned no
	// violations. The header's shape did not change at 1.0 — a payload length, an int32
	// version, then the name — so one parser reads `.fwl` and `.fwl2` alike (03 §4.2).
	Info WorldInfo
}

// Install lists the world's staged files against the names they take under worlds_local/ for
// an instance configured to load worldName. A pair is renamed to that name; a directory keeps
// its own file names inside a directory renamed to it, because 1.0 loads the directory by name
// and the files inside it carry a save counter the panel has no business rewriting.
func (w *UploadedWorld) Install(stagingDir, worldName string) []StagedFile {
	out := make([]StagedFile, 0, len(w.Files))
	for _, rel := range w.Files {
		name := worldName + filepath.Ext(rel)
		if w.Directory {
			name = filepath.Join(worldName, filepath.Base(rel))
		}
		out = append(out, StagedFile{Path: filepath.Join(stagingDir, rel), Name: name})
	}
	return out
}

// ValidateImport applies 03 §4.1's rules 1, 3, 4 and 5 to a staging directory, the only thing
// standing between an arbitrary upload and a user's worlds/ directory. It reads the staged
// files but writes nothing; rule 6's snapshot and the move are the caller's, after this returns
// clean.
//
// Rules 1 and 2 are satisfied by the scan itself: it recognises a world in either layout
// wherever in the upload it sits, so `worlds_local/`, the Windows LocalLow path and a legacy
// `worlds/` all flatten to the same answer and no layout needs naming (ADR-179).
//
// allowBackupVariant is rule 5's explicit choice, a parameter rather than a heuristic since the
// panel cannot infer intent from the bytes.
func ValidateImport(stagingDir string, allowBackupVariant bool) (*UploadedWorld, []ImportViolation) {
	scan, err := backup.ScanWorlds(stagingDir)
	if err != nil {
		return nil, []ImportViolation{{RulePairIncomplete, "The upload could not be read."}}
	}
	if v := unsafeNames(scan); len(v) > 0 {
		return nil, v
	}

	names := make([]string, 0, len(scan))
	for name := range scan {
		names = append(names, name)
	}
	slices.Sort(names)

	if len(names) == 0 {
		return nil, []ImportViolation{{
			Rule: RulePairIncomplete,
			Detail: "No world was found. Upload a world folder, or the .db and .fwl pair an " +
				"older Valheim wrote, or a zip containing either.",
		}}
	}
	if len(names) > 1 {
		return nil, []ImportViolation{{RulePairIncomplete, fmt.Sprintf(
			"Found %d worlds (%s). Upload one world at a time.", len(names), strings.Join(names, ", "))}}
	}

	name := names[0]
	found := scan[name]
	if !found.Complete() {
		return nil, []ImportViolation{{RulePairIncomplete, halfAWorld(name, found)}}
	}
	if !allowBackupVariant && backupVariant.MatchString(name) {
		return nil, []ImportViolation{{RuleBackupVariant, fmt.Sprintf(
			"%q is one of the game's own rolling backups, not the live world. "+
				"Import it only if you mean to restore that older state.", name)}}
	}

	w := &UploadedWorld{Basename: name, Directory: found.Directory, Files: found.Files}
	w.DBPath, w.FWLPath = halfPaths(stagingDir, found)
	if v := checkSize(name, found); v != nil {
		return nil, []ImportViolation{*v}
	}

	raw, err := os.ReadFile(w.FWLPath)
	if err != nil {
		return nil, []ImportViolation{{RuleNotAWorldFile, fmt.Sprintf(
			"%s's world header could not be read.", name)}}
	}
	info, err := ParseFWL(raw)
	if err != nil {
		return nil, []ImportViolation{{RuleNotAWorldFile, fmt.Sprintf(
			"%s's world header is not a world metadata file: %v", name, err)}}
	}
	w.Info = info
	return w, nil
}

// halfPaths locates the world's two halves inside the staging directory.
func halfPaths(stagingDir string, found *backup.WorldFiles) (db, fwl string) {
	for _, rel := range found.Files {
		_, _, part := backup.ClassifyWorldFile(rel)
		switch part {
		case backup.PartData:
			db = filepath.Join(stagingDir, rel)
		case backup.PartHeader:
			fwl = filepath.Join(stagingDir, rel)
		case backup.PartOther:
		}
	}
	return db, fwl
}

// halfAWorld is rule 1: the pair is the unit, in either layout. Naming the missing half is the
// one message that tells the user what to do next.
func halfAWorld(name string, found *backup.WorldFiles) string {
	missing, present := ".fwl", ".db"
	if found.DataBytes < 0 {
		missing, present = ".db", ".fwl"
	}
	if found.Directory {
		missing, present = "_main.<n>.fwl2", "_main.<n>.db2"
		if found.DataBytes < 0 {
			missing, present = "_main.<n>.db2", "_main.<n>.fwl2"
		}
	}
	return fmt.Sprintf("%s has a %s with no matching %s. A world is both together.",
		name, present, missing)
}

// checkSize is rule 5's sanity floor. The pre-1.0 floor was measured against a file holding
// the whole world; a 1.0 `.db2` sits beside chunk files that hold most of it and no floor for
// it has been measured (Q56), so that layout is held only to "not empty".
func checkSize(name string, found *backup.WorldFiles) *ImportViolation {
	floor := int64(minDBBytes)
	half := name + ".db"
	if found.Directory {
		floor, half = 1, name+"/_main.<n>.db2"
	}
	if found.DataBytes < floor {
		return &ImportViolation{RuleDBTooSmall, fmt.Sprintf(
			"%s is %d bytes, which is too small to be a world.", half, found.DataBytes)}
	}
	return nil
}

// unsafeNames refuses a world whose name could escape when it is later joined onto worlds/,
// before anything is moved — the same root discipline WorldPath applies (B5), applied to the
// name an upload chose rather than one a user typed.
func unsafeNames(scan backup.WorldScan) []ImportViolation {
	var violations []ImportViolation
	for name := range scan {
		if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			violations = append(violations, ImportViolation{RuleUnsafeName, fmt.Sprintf(
				"%q is not a usable world name.", name)})
		}
	}
	return violations
}
