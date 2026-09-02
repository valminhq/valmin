package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shipped is the shape denikson-BepInExPack_Valheim-5.4.2333 actually ships (read from the
// package 1 Sep 2026), trimmed to the sections that matter. It already reads `true`, which
// is why the write path is the rare one — and why the no-write case is asserted first.
const shipped = `## Settings file was created by plugin BepInEx v5.4.23.3
## Plugin GUID: BepInEx

[Logging.Console]

## Enables showing a console for log output.
# Setting type: Boolean
# Default value: false
Enabled = true

## If enabled, will prevent closing the console.
# Setting type: Boolean
# Default value: false
PreventClose = true

[Logging.Disk]

## Include unity log messages in log file output.
# Setting type: Boolean
# Default value: false
Enabled = false
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "BepInEx.cfg")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestEnsureConsoleLoggingWritesNothingWhenAlreadyTrue is the common case: the pack ships
// `true`, so the panel must leave the file completely alone rather than rewrite it to
// identical bytes.
func TestEnsureConsoleLoggingWritesNothingWhenAlreadyTrue(t *testing.T) {
	path := write(t, shipped)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureConsoleLogging(path)
	if err != nil {
		t.Fatalf("EnsureConsoleLogging: %v", err)
	}
	if changed {
		t.Error("changed = true on a file that already reads true")
	}
	if got := read(t, path); got != shipped {
		t.Errorf("file changed:\n%s", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the file was rewritten; a no-op must not touch it at all")
	}
}

// TestEnsureConsoleLoggingChangesExactlyOneLine is B10. Everything the operator put in the
// file — comments, blank lines, section order, unknown keys, and the identically-named
// Enabled key in another section — has to survive byte for byte.
func TestEnsureConsoleLoggingChangesExactlyOneLine(t *testing.T) {
	before := strings.Replace(shipped, "Enabled = true", "Enabled = false", 1)
	path := write(t, before)

	changed, err := EnsureConsoleLogging(path)
	if err != nil {
		t.Fatalf("EnsureConsoleLogging: %v", err)
	}
	if !changed {
		t.Fatal("changed = false on a file that read false")
	}

	got := read(t, path)
	if got != shipped {
		t.Errorf("file after the edit:\n%s\nwant:\n%s", got, shipped)
	}
	// Stated separately from the whole-file compare so a failure says which rule broke.
	if !strings.Contains(got, "[Logging.Disk]\n\n## Include unity log messages in log file output.") {
		t.Error("the other section's comments did not survive")
	}
	if strings.Count(got, "Enabled = false") != 1 {
		t.Error("the Enabled key in [Logging.Disk] was changed too")
	}
	if strings.Count(got, "# Default value: false") != 3 {
		t.Error("a comment mentioning the value was rewritten")
	}
}

// TestEnableConsolePreservesFormatting: the value token is spliced out, so whatever spacing,
// line ending or trailing content the line had is carried through.
func TestEnableConsolePreservesFormatting(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no spaces", "[Logging.Console]\nEnabled=false\n", "[Logging.Console]\nEnabled=true\n"},
		{"wide spacing", "[Logging.Console]\nEnabled   =    false\n", "[Logging.Console]\nEnabled   =    true\n"},
		{"crlf", "[Logging.Console]\r\nEnabled = false\r\n", "[Logging.Console]\r\nEnabled = true\r\n"},
		{"no trailing newline", "[Logging.Console]\nEnabled = false", "[Logging.Console]\nEnabled = true"},
		{"tabs", "[Logging.Console]\n\tEnabled\t=\tfalse\n", "[Logging.Console]\n\tEnabled\t=\ttrue\n"},
		{"mixed case value", "[Logging.Console]\nEnabled = False\n", "[Logging.Console]\nEnabled = true\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := enableConsole(tt.in)
			if err != nil {
				t.Fatalf("enableConsole: %v", err)
			}
			if !changed || got != tt.want {
				t.Errorf("enableConsole(%q) = %q, %v; want %q, true", tt.in, got, changed, tt.want)
			}
		})
	}
}

// TestEnsureConsoleLoggingReportsAMissingKey. `↯` BepInEx's own default for this key is
// false, so an absent key is a server that will load its plugins and tell the panel
// nothing — 03 §5.2's silent failure. It is reported rather than passed over, and this
// package will not invent a section it was not asked to write (that is M3's AST).
func TestEnsureConsoleLoggingReportsAMissingKey(t *testing.T) {
	for _, body := range []string{
		"[Logging.Disk]\nEnabled = false\n",
		"[Logging.Console]\nPreventClose = true\n",
		"",
	} {
		path := write(t, body)
		changed, err := EnsureConsoleLogging(path)
		if !errors.Is(err, ErrConsoleKeyMissing) {
			t.Errorf("EnsureConsoleLogging(%q) = %v, %v; want ErrConsoleKeyMissing", body, changed, err)
		}
		if got := read(t, path); got != body {
			t.Errorf("the file was modified: %q", got)
		}
	}
}

// TestEnableConsoleIgnoresCommentedKeys: the section's own comments name the key and its
// default, so a matcher that did not skip comment lines would rewrite the documentation.
func TestEnableConsoleIgnoresCommentedKeys(t *testing.T) {
	in := "[Logging.Console]\n# Enabled = false\n## Enabled = false\nEnabled = false\n"
	want := "[Logging.Console]\n# Enabled = false\n## Enabled = false\nEnabled = true\n"

	got, changed, err := enableConsole(in)
	if err != nil || !changed || got != want {
		t.Errorf("enableConsole = %q, %v, %v; want %q, true, nil", got, changed, err, want)
	}
}

// TestEnableConsoleLeavesAnUnrecognisedValueAlone: the panel changes `false` to `true` and
// makes no other decision. A value it does not understand is the operator's, and
// overwriting it with a guess is what a surgical edit exists to avoid.
func TestEnableConsoleLeavesAnUnrecognisedValueAlone(t *testing.T) {
	in := "[Logging.Console]\nEnabled = maybe\n"
	got, changed, err := enableConsole(in)
	if err != nil {
		t.Fatalf("enableConsole: %v", err)
	}
	if changed || got != in {
		t.Errorf("enableConsole = %q, %v; want it untouched", got, changed)
	}
}

// TestEnableConsoleStopsAtTheNextSection: a later section's Enabled must not be taken as
// this one's, which is what would happen if the section were tracked as "everything after
// the header".
func TestEnableConsoleStopsAtTheNextSection(t *testing.T) {
	in := "[Logging.Console]\nPreventClose = true\n\n[Logging.Disk]\nEnabled = false\n"
	if _, _, err := enableConsole(in); !errors.Is(err, ErrConsoleKeyMissing) {
		t.Errorf("enableConsole = %v, want ErrConsoleKeyMissing", err)
	}
}
