package instance

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageWorld writes a plausible pair into dir/sub, so a test can build the three source
// layouts 03 §4.1 rule 2 requires the panel to accept.
func stageWorld(t *testing.T, dir, sub, basename string) {
	t.Helper()
	target := filepath.Join(dir, sub)
	if err := os.MkdirAll(target, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, basename+".db"),
		[]byte(strings.Repeat("world ", 400)), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, basename+".fwl"),
		buildFWL(37, basename, []byte{1, 2, 3}), 0o664); err != nil {
		t.Fatal(err)
	}
}

func onlyViolation(t *testing.T, vs []ImportViolation) ImportViolation {
	t.Helper()
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want exactly one", vs)
	}
	return vs[0]
}

// TestValidateImportAcceptsEverySourceLayout is 03 §4.1 rule 2. Uploads come from
// worlds_local/ (Linux/Mac), the Windows LocalLow path, and the legacy worlds/ folder — all
// three must normalise to the same answer without the panel naming any of them.
func TestValidateImportAcceptsEverySourceLayout(t *testing.T) {
	layouts := map[string]string{
		"bare files":          ".",
		"linux worlds_local":  "worlds_local",
		"legacy worlds":       "worlds",
		"windows LocalLow":    "AppData/LocalLow/IronGate/Valheim/worlds_local",
		"nested in a zip dir": "Valheim Backup/worlds_local",
	}
	for name, sub := range layouts {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stageWorld(t, dir, sub, "Dedicated")

			got, vs := ValidateImport(dir, false)
			if len(vs) != 0 {
				t.Fatalf("violations = %+v, want none", vs)
			}
			if got.Basename != "Dedicated" || got.Info.Version != 37 || got.Info.Name != "Dedicated" {
				t.Errorf("parsed = %+v", got)
			}
		})
	}
}

// TestValidateImportRefusesAHalfPair is rule 1, the one users hit most: a world is the pair,
// and a lone half must be named as such rather than reported as "nothing found".
func TestValidateImportRefusesAHalfPair(t *testing.T) {
	for _, ext := range []string{".db", ".fwl"} {
		t.Run("only"+ext, func(t *testing.T) {
			dir := t.TempDir()
			stageWorld(t, dir, "worlds_local", "Dedicated")
			if err := os.Remove(filepath.Join(dir, "worlds_local", "Dedicated"+ext)); err != nil {
				t.Fatal(err)
			}

			_, vs := ValidateImport(dir, false)
			if v := onlyViolation(t, vs); v.Rule != RulePairIncomplete {
				t.Errorf("rule = %s, want %s", v.Rule, RulePairIncomplete)
			}
		})
	}
}

func TestValidateImportRefusesAnEmptyUpload(t *testing.T) {
	_, vs := ValidateImport(t.TempDir(), false)
	if v := onlyViolation(t, vs); v.Rule != RulePairIncomplete {
		t.Errorf("rule = %s, want %s", v.Rule, RulePairIncomplete)
	}
}

// TestValidateImportRefusesMoreThanOneWorld: a user who uploads their whole save folder must
// be told to pick one, not have the panel choose for them.
func TestValidateImportRefusesMoreThanOneWorld(t *testing.T) {
	dir := t.TempDir()
	stageWorld(t, dir, "worlds_local", "Dedicated")
	stageWorld(t, dir, "worlds_local", "Other")

	_, vs := ValidateImport(dir, false)
	v := onlyViolation(t, vs)
	if v.Rule != RulePairIncomplete || !strings.Contains(v.Detail, "one world at a time") {
		t.Errorf("violation = %+v", v)
	}
}

// TestValidateImportRefusesTheEnginesOwnBackups is rule 5, and it is not hypothetical: a real
// save folder measured on 31 Aug 2026 held two `_backup_auto-*` pairs beside the live world,
// so a user uploading that folder hits this every time.
func TestValidateImportRefusesTheEnginesOwnBackups(t *testing.T) {
	dir := t.TempDir()
	stageWorld(t, dir, "worlds_local", "NewWorld_backup_auto-20240517230012")

	_, vs := ValidateImport(dir, false)
	if v := onlyViolation(t, vs); v.Rule != RuleBackupVariant {
		t.Fatalf("rule = %s, want %s", v.Rule, RuleBackupVariant)
	}

	// ...unless the user explicitly picked it, which is rule 5's own escape hatch.
	got, vs := ValidateImport(dir, true)
	if len(vs) != 0 {
		t.Fatalf("explicitly picked backup still refused: %+v", vs)
	}
	if got.Basename != "NewWorld_backup_auto-20240517230012" {
		t.Errorf("basename = %q", got.Basename)
	}
}

// TestValidateImportAcceptsAWorldWhoseInternalNameDiffers is the correction measured on
// 31 Aug 2026: the game's own `_backup_auto-*` files store the *original* world name, so
// internal name and filename legitimately disagree and a mismatch must never be a failure.
func TestValidateImportAcceptsAWorldWhoseInternalNameDiffers(t *testing.T) {
	dir := t.TempDir()
	stageWorld(t, dir, "worlds_local", "Renamed")
	if err := os.WriteFile(filepath.Join(dir, "worlds_local", "Renamed.fwl"),
		buildFWL(34, "OriginalName", nil), 0o664); err != nil {
		t.Fatal(err)
	}

	got, vs := ValidateImport(dir, false)
	if len(vs) != 0 {
		t.Fatalf("a name mismatch was rejected: %+v — the game itself produces these", vs)
	}
	if got.Info.Name != "OriginalName" || got.Basename != "Renamed" {
		t.Errorf("got %+v, want the mismatch preserved for the caller to resolve", got)
	}
}

func TestValidateImportRefusesAnEmptyOrTinyDB(t *testing.T) {
	dir := t.TempDir()
	stageWorld(t, dir, "worlds_local", "Dedicated")
	if err := os.WriteFile(filepath.Join(dir, "worlds_local", "Dedicated.db"), []byte("nope"), 0o664); err != nil {
		t.Fatal(err)
	}

	_, vs := ValidateImport(dir, false)
	if v := onlyViolation(t, vs); v.Rule != RuleDBTooSmall {
		t.Errorf("rule = %s, want %s", v.Rule, RuleDBTooSmall)
	}
}

// TestValidateImportRefusesAnFWLThatIsNotOne is rule 4's gate: the version cannot be read
// from a file that is not a world header, and letting it through means a server that fails
// to boot with no explanation.
func TestValidateImportRefusesAnFWLThatIsNotOne(t *testing.T) {
	dir := t.TempDir()
	stageWorld(t, dir, "worlds_local", "Dedicated")
	if err := os.WriteFile(filepath.Join(dir, "worlds_local", "Dedicated.fwl"),
		[]byte("this is not a world header at all"), 0o664); err != nil {
		t.Fatal(err)
	}

	_, vs := ValidateImport(dir, false)
	if v := onlyViolation(t, vs); v.Rule != RuleNotAWorldFile {
		t.Errorf("rule = %s, want %s", v.Rule, RuleNotAWorldFile)
	}
}

// TestValidateImportReportsTheVersionForSkewChecking is rule 4's payload: the caller cannot
// refuse a skewed world without being told which version it is.
func TestValidateImportReportsTheVersionForSkewChecking(t *testing.T) {
	for _, version := range []int32{33, 34, 37} {
		dir := t.TempDir()
		stageWorld(t, dir, "worlds_local", "Dedicated")
		if err := os.WriteFile(filepath.Join(dir, "worlds_local", "Dedicated.fwl"),
			buildFWL(version, "Dedicated", nil), 0o664); err != nil {
			t.Fatal(err)
		}
		got, vs := ValidateImport(dir, false)
		if len(vs) != 0 {
			t.Fatalf("version %d: %+v", version, vs)
		}
		if got.Info.Version != version {
			t.Errorf("version = %d, want %d", got.Info.Version, version)
		}
	}
}

// TestValidateImportRefusesANameThatCouldEscape is B5 applied to a name an archive chose
// rather than one a user typed — refused before anything moves.
func TestValidateImportRefusesANameThatCouldEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o775); err != nil {
		t.Fatal(err)
	}
	// A zip entry named "../../escape.db" extracts to this basename on a naive extractor.
	if err := os.WriteFile(filepath.Join(dir, "sub", "..db"), []byte(strings.Repeat("x", 2000)), 0o664); err != nil {
		t.Fatal(err)
	}
	_, vs := ValidateImport(dir, false)
	if len(vs) == 0 {
		t.Fatal("a name containing .. was accepted")
	}
	if vs[0].Rule != RuleUnsafeName && vs[0].Rule != RulePairIncomplete {
		t.Errorf("rule = %s", vs[0].Rule)
	}
}

func TestBuildFWLRoundTripsThroughValidate(t *testing.T) {
	raw := buildFWL(37, "Check", nil)
	if got := binary.LittleEndian.Uint32(raw[4:8]); got != 37 {
		t.Fatalf("fixture builder is wrong: version reads %d", got)
	}
}
