package config

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestCorpusRoundTripsByteIdentically asserts every corpus file survives parse and serialise
// byte for byte (03 §9 rule 6).
func TestCorpusRoundTripsByteIdentically(t *testing.T) {
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := Parse(raw).Bytes(); !bytes.Equal(got, raw) {
				g, w := firstDiff(got, raw)
				t.Errorf("round trip changed the file:\n got %q\nwant %q", g, w)
			}
		})
	}
}

// TestSetTouchesOneLineAndOnlyItsValue asserts, for every setting in the corpus, that
// rewriting its own value changes nothing and a new value changes only its line.
func TestSetTouchesOneLineAndOnlyItsValue(t *testing.T) {
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range Parse(raw).Settings() {
				doc := Parse(raw)
				if err := doc.Set(s.Section, s.Key, s.Value); err != nil {
					t.Fatalf("[%s] %s: %v", s.Section, s.Key, err)
				}
				if !bytes.Equal(doc.Bytes(), raw) {
					t.Errorf("[%s] %s: rewriting the value it already holds changed the file",
						s.Section, s.Key)
				}

				doc = Parse(raw)
				if err := doc.Set(s.Section, s.Key, "SENTINEL"); err != nil {
					t.Fatalf("[%s] %s: %v", s.Section, s.Key, err)
				}
				assertOneLineChanged(t, raw, doc.Bytes(), s)
			}
		})
	}
}

// assertOneLineChanged checks the edit produced a one-line diff on the named setting.
func assertOneLineChanged(t *testing.T, before, after []byte, s Setting) {
	t.Helper()
	was := strings.Split(string(before), "\n")
	is := strings.Split(string(after), "\n")
	if len(was) != len(is) {
		t.Fatalf("[%s] %s: line count went from %d to %d", s.Section, s.Key, len(was), len(is))
	}
	var changed []int
	for i := range was {
		if was[i] != is[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("[%s] %s: %d lines changed, want 1", s.Section, s.Key, len(changed))
	}
	line := is[changed[0]]
	if !strings.Contains(line, s.Key) || !strings.Contains(line, "SENTINEL") {
		t.Errorf("[%s] %s: the changed line is %q", s.Section, s.Key, line)
	}
}

// TestParseReadsTheAwkwardShapes asserts the lexical decisions a `.cfg` forces.
func TestParseReadsTheAwkwardShapes(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		section string
		key     string
		want    string
	}{
		{"value containing equals", "edge/value-with-equals.cfg", "General", "Query", "mode=hard&raids=on"},
		{"value that is an equals", "edge/value-with-equals.cfg", "General", "Separator", "="},
		{"empty value", "edge/value-with-equals.cfg", "General", "Prefix", ""},
		{"key before any section", "edge/key-before-section.cfg", "", "Language", "de"},
		{"duplicate key takes the first", "edge/duplicate-key.cfg", "General", "Retries", "3"},
		{"same key in another section", "edge/duplicate-key.cfg", "Network", "Retries", "5"},
		{"key matched case-insensitively", "edge/duplicate-key.cfg", "Network", "retries", "5"},
		{"padding around equals is not the value", "edge/trailing-whitespace.cfg", "General", "Workers", "4"},
		{"setting with no metadata", "edge/unknown-metadata.cfg", "General", "Threshold", "0.25"},
		{"key in a file of mixed line endings", "plugin/BepInEx.cfg", "Logging.Disk", "AppendLog", "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseFile(t, tt.file).Get(tt.section, tt.key)
			if !ok {
				t.Fatalf("[%s] %s is not in %s", tt.section, tt.key, tt.file)
			}
			if got != tt.want {
				t.Errorf("[%s] %s = %q, want %q", tt.section, tt.key, got, tt.want)
			}
		})
	}
}

// TestSetOnADuplicateKeyChangesOnlyTheFirst asserts the shadowed assignment is left alone;
// the server never reads it.
func TestSetOnADuplicateKeyChangesOnlyTheFirst(t *testing.T) {
	doc := parseFile(t, "edge/duplicate-key.cfg")
	if err := doc.Set("General", "Retries", "7"); err != nil {
		t.Fatal(err)
	}
	out := string(doc.Bytes())
	if !strings.Contains(out, "Retries = 7") {
		t.Error("the first assignment was not updated")
	}
	if !strings.Contains(out, "Retries = 9") {
		t.Error("the shadowed second assignment was rewritten; it is not the one in effect")
	}
}

// TestSetIsRefusedRatherThanGuessed asserts an unknown setting and a value carrying a newline
// are both refused, and neither modifies the document.
func TestSetIsRefusedRatherThanGuessed(t *testing.T) {
	tests := []struct {
		name             string
		section, key, in string
		want             error
	}{
		{"unknown key", "General", "NotAKey", "1", ErrNoSuchSetting},
		{"unknown section", "Nowhere", "Enabled", "true", ErrNoSuchSetting},
		{"value with a newline", "General", "Enabled", "true\nEnabled = false", ErrValueSpansLines},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseFile(t, "plugin/com.example.minimal.cfg")
			before := doc.Bytes()
			if err := doc.Set(tt.section, tt.key, tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("Set = %v, want %v", err, tt.want)
			}
			if !bytes.Equal(doc.Bytes(), before) {
				t.Error("a refused Set still modified the document")
			}
		})
	}
}

// TestSettingsCarryTheirCommentBlock asserts a setting is handed the comment run above it and
// no more; the block is what carries its type and constraints.
func TestSettingsCarryTheirCommentBlock(t *testing.T) {
	settings := parseFile(t, "plugin/com.example.minimal.cfg").Settings()
	if len(settings) != 1 {
		t.Fatalf("got %d settings, want 1", len(settings))
	}
	want := []string{
		"## Whether the mod is active.",
		"# Setting type: Boolean",
		"# Default value: true",
	}
	if !slices.Equal(settings[0].Comments, want) {
		t.Errorf("comments = %q, want %q", settings[0].Comments, want)
	}
}

// TestABlankLineEndsACommentBlock asserts the file header does not leak into the first
// setting's comments.
func TestABlankLineEndsACommentBlock(t *testing.T) {
	settings := parseFile(t, "plugin/com.example.minimal.cfg").Settings()
	for _, c := range settings[0].Comments {
		if strings.Contains(c, "Plugin GUID") {
			t.Errorf("the file header leaked into the first setting's comments: %q", c)
		}
	}
}

// TestSettingsListsTheOnesInEffect asserts settings come back in file order with a shadowed
// duplicate left out, since Set cannot address it.
func TestSettingsListsTheOnesInEffect(t *testing.T) {
	settings := parseFile(t, "edge/duplicate-key.cfg").Settings()
	got := make([]string, 0, len(settings))
	for _, s := range settings {
		got = append(got, s.Section+"."+s.Key+"="+s.Value)
	}
	want := []string{"General.Retries=3", "Network.Retries=5"}
	if !slices.Equal(got, want) {
		t.Errorf("settings = %v, want %v", got, want)
	}
}

// FuzzParseRoundTrip asserts parse-then-serialise is the identity over arbitrary bytes.
func FuzzParseRoundTrip(f *testing.F) {
	for _, path := range corpusFiles(f) {
		if raw, err := os.ReadFile(path); err == nil {
			f.Add(raw)
		}
	}
	f.Add([]byte("[A]\r\nk = v"))
	f.Add([]byte("= v\n[]\n#\n"))
	f.Add([]byte("\x00\xff = \xef\xbb\xbf\n"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		if got := Parse(raw).Bytes(); !bytes.Equal(got, raw) {
			g, w := firstDiff(got, raw)
			t.Errorf("round trip changed the input:\n got %q\nwant %q", g, w)
		}
	})
}

// FuzzSetKeepsEveryOtherByte asserts an edit replaces one value token and leaves every other
// byte alone, whatever the file looks like (03 §9 rule 2).
func FuzzSetKeepsEveryOtherByte(f *testing.F) {
	f.Add([]byte("[A]\nk = v\n"), "w")
	f.Add([]byte("[A]\r\n  k\t=\tv  \r\n"), "")
	f.Add([]byte("k = a=b\n"), "c=d")

	f.Fuzz(func(t *testing.T, raw []byte, value string) {
		if strings.Contains(value, "\n") {
			t.Skip("rejected by Set, and covered by TestSetIsRefusedRatherThanGuessed")
		}
		doc := Parse(raw)
		settings := doc.Settings()
		if len(settings) == 0 {
			return
		}
		s := settings[0]
		if err := doc.Set(s.Section, s.Key, value); err != nil {
			t.Fatalf("[%s] %s: %v", s.Section, s.Key, err)
		}
		edited := doc.Bytes()
		if got, want := lineCount(edited), lineCount(raw); got != want {
			t.Fatalf("the line count went from %d to %d: %q became %q", want, got, raw, edited)
		}
		// Restoring the old value can only reproduce the file if nothing else moved.
		if err := doc.Set(s.Section, s.Key, s.Value); err != nil {
			t.Fatalf("[%s] %s: %v", s.Section, s.Key, err)
		}
		if !bytes.Equal(doc.Bytes(), raw) {
			g, w := firstDiff(doc.Bytes(), raw)
			t.Errorf("bytes outside the value changed:\n got %q\nwant %q", g, w)
		}
	})
}

// lineCount counts the lines a serialised document holds.
func lineCount(b []byte) int {
	return bytes.Count(b, []byte("\n")) + 1
}

func parseFile(t *testing.T, name string) *Document {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(raw)
}

// firstDiff windows two slices around the first byte they differ at, so a failure on a large
// file names the line rather than printing the file.
func firstDiff(got, want []byte) (gotWindow, wantWindow string) {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	start := max(i-40, 0)
	return string(got[start:min(i+40, len(got))]), string(want[start:min(i+40, len(want))])
}
