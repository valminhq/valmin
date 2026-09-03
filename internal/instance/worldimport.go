package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WorldsLocalDir is the subdirectory of worlds/ the game keeps saves in (03 §4). Uploads
// arrive from three different source layouts and are all normalised into this one.
const WorldsLocalDir = "worlds_local"

// minDBBytes is 03 §4.1 rule 5's "sanity, not trust" floor on a world database. A real
// world is hundreds of kilobytes at minimum — the smallest measured was 998 KB — but the
// check exists to catch an empty or truncated upload, not to police world size, so it sits
// far below anything real.
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

// backupVariant matches the engine's own rolling saves. 03 §4.1 rule 5 says to reject these
// unless the user explicitly picks one: they are the *previous* state of a world, and a user
// who uploads a whole save folder almost never means to restore one.
//
// `.old` is matched as its own suffix rather than folded in here, because a file named
// `World.db.old` has already lost the `.db` extension the pair check keys on.
var backupVariant = regexp.MustCompile(`_backup_auto-\d+$`)

// UploadedWorld is one candidate pair, already staged on disk.
type UploadedWorld struct {
	// Basename is the shared stem, e.g. "Dedicated" for Dedicated.db + Dedicated.fwl.
	Basename string
	DBPath   string
	FWLPath  string
	// Info is the parsed `.fwl` header, valid only once ValidateImport has returned no
	// violations.
	Info WorldInfo
}

// ValidateImport applies 03 §4.1's rules 1, 3, 4 and 5 to a staging directory, and is the
// only thing standing between an arbitrary upload and a user's worlds/ directory. It reads
// the staged files but writes nothing: rule 6's snapshot and the move itself are the
// caller's, and both happen only after this returns clean.
//
// allowBackupVariant is the "unless explicitly picked" of rule 5, and it is a parameter
// rather than a heuristic because the panel cannot infer intent from the bytes.
func ValidateImport(stagingDir string, allowBackupVariant bool) (*UploadedWorld, []ImportViolation) {
	pairs, violations := findPairs(stagingDir)
	if len(violations) > 0 {
		return nil, violations
	}
	if len(pairs) == 0 {
		return nil, []ImportViolation{{
			Rule: RulePairIncomplete,
			Detail: "No world was found. A world is a matching .db and .fwl pair; " +
				"upload both, or a zip containing both.",
		}}
	}
	if len(pairs) > 1 {
		return nil, []ImportViolation{{RulePairIncomplete, fmt.Sprintf(
			"Found %d worlds (%s). Upload one world at a time.", len(pairs), strings.Join(basenames(pairs), ", "))}}
	}

	w := pairs[0]
	if !allowBackupVariant && backupVariant.MatchString(w.Basename) {
		return nil, []ImportViolation{{RuleBackupVariant, fmt.Sprintf(
			"%q is one of the game's own rolling backups, not the live world. "+
				"Import it only if you mean to restore that older state.", w.Basename)}}
	}

	if fi, err := os.Stat(w.DBPath); err != nil || fi.Size() < minDBBytes {
		size := int64(-1)
		if err == nil {
			size = fi.Size()
		}
		return nil, []ImportViolation{{RuleDBTooSmall, fmt.Sprintf(
			"%s.db is %d bytes, which is too small to be a world.", w.Basename, size)}}
	}

	raw, err := os.ReadFile(w.FWLPath)
	if err != nil {
		return nil, []ImportViolation{{RuleNotAWorldFile, fmt.Sprintf("%s.fwl could not be read.", w.Basename)}}
	}
	info, err := ParseFWL(raw)
	if err != nil {
		return nil, []ImportViolation{{RuleNotAWorldFile, fmt.Sprintf(
			"%s.fwl is not a world metadata file: %v", w.Basename, err)}}
	}
	w.Info = info
	return &w, nil
}

// findPairs walks a staging tree and groups .db/.fwl files by basename, which is how rules 1
// and 2 are satisfied at once: the walk flattens `worlds_local/`, the Windows LocalLow path
// and the legacy `worlds/` layout into the same set of pairs, so no layout needs naming.
func findPairs(stagingDir string) ([]UploadedWorld, []ImportViolation) {
	dbs, fwls := map[string]string{}, map[string]string{}
	var violations []ImportViolation

	err := filepath.Walk(stagingDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
			return err
		}
		base := filepath.Base(path)
		stem := strings.TrimSuffix(strings.TrimSuffix(base, ".db"), ".fwl")
		// A name that could escape when it is later joined onto worlds/ is refused here,
		// before anything is moved — the same root discipline WorldPath applies (B5), applied
		// to the name an archive entry chose rather than one a user typed.
		if stem != base && (strings.ContainsAny(stem, `/\`) || strings.Contains(stem, "..") || stem == "") {
			violations = append(violations, ImportViolation{RuleUnsafeName, fmt.Sprintf(
				"%q is not a usable world name.", base)})
			return nil
		}
		switch {
		case strings.HasSuffix(base, ".db"):
			dbs[stem] = path
		case strings.HasSuffix(base, ".fwl"):
			fwls[stem] = path
		}
		return nil
	})
	if err != nil {
		return nil, []ImportViolation{{RulePairIncomplete, "The upload could not be read."}}
	}
	if len(violations) > 0 {
		return nil, violations
	}

	return pairUp(dbs, fwls)
}

// pairUp is rule 1: the pair is the unit. A lone half is named explicitly rather than
// reported as "no world found", because "you sent a .db without its .fwl" is the one message
// that tells the user what to do next.
func pairUp(dbs, fwls map[string]string) ([]UploadedWorld, []ImportViolation) {
	var violations []ImportViolation
	out := make([]UploadedWorld, 0, len(dbs))
	for stem, db := range dbs {
		fwl, ok := fwls[stem]
		if !ok {
			violations = append(violations, ImportViolation{RulePairIncomplete, fmt.Sprintf(
				"%s.db has no matching %s.fwl. A world is both files together.", stem, stem)})
			continue
		}
		out = append(out, UploadedWorld{Basename: stem, DBPath: db, FWLPath: fwl})
	}
	for stem := range fwls {
		if _, ok := dbs[stem]; !ok {
			violations = append(violations, ImportViolation{RulePairIncomplete, fmt.Sprintf(
				"%s.fwl has no matching %s.db. A world is both files together.", stem, stem)})
		}
	}
	if len(violations) > 0 {
		return nil, violations
	}
	return out, nil
}

func basenames(w []UploadedWorld) []string {
	out := make([]string, len(w))
	for i := range w {
		out[i] = w[i].Basename
	}
	return out
}
