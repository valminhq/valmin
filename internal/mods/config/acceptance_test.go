package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEverySettingSurvivesTheWholeChain is the milestone's stated guarantee run end to end,
// over every file in the corpus and every setting in every file: parse, serialise
// byte-identically, project to a schema, change one setting through the same Apply a request
// takes, and come back with exactly the line that was asked for and nothing else.
//
// The parts are covered on their own elsewhere. What this adds is the join: it goes through
// Schema and Apply — typed values, validated against the metadata — rather than Set with a
// string, which is the path no unit test walks from end to end.
func TestEverySettingSurvivesTheWholeChain(t *testing.T) {
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := Parse(raw).Bytes(); !bytes.Equal(got, raw) {
				g, w := firstDiff(got, raw)
				t.Fatalf("round trip changed the file:\n got %q\nwant %q", g, w)
			}

			schema := Parse(raw).Schema(filepath.Base(path))
			for _, section := range schema.Sections {
				for _, item := range section.Settings {
					field := section.Name + "." + item.Key
					t.Run(field, func(t *testing.T) {
						applyAndCompare(t, raw, field, &item)
					})
				}
			}
		})
	}
}

// applyAndCompare writes one setting and checks what the file has to say about it.
func applyAndCompare(t *testing.T, raw []byte, field string, item *SchemaItem) {
	t.Helper()

	// Writing back the value the schema reported must be a no-op. A projection that
	// reformatted anything on the way out would show up here as a diff on a value nobody
	// changed, which is the shape of a save that quietly rewrites a user's file.
	same := Parse(raw)
	if errs := same.Apply(map[string]any{field: item.Current}); len(errs) > 0 {
		t.Fatalf("re-applying the projected value: %+v", errs)
	}
	if !bytes.Equal(same.Bytes(), raw) {
		g, w := firstDiff(same.Bytes(), raw)
		t.Errorf("writing the value it already holds changed the file:\n got %q\nwant %q", g, w)
	}

	next, ok := anotherValue(item)
	if !ok {
		// A setting whose type admits exactly one legal value. The no-op above is the whole
		// claim for it, and inventing a second value would only test the validator.
		return
	}

	doc := Parse(raw)
	if errs := doc.Apply(map[string]any{field: next}); len(errs) > 0 {
		t.Fatalf("apply %v (%s, widget %s): %+v", next, item.Type, item.Widget, errs)
	}
	after := doc.Bytes()

	changed := changedLines(string(raw), string(after))
	if len(changed) != 1 {
		t.Fatalf("%d lines changed, want 1", len(changed))
	}
	line := strings.Split(string(after), "\n")[changed[0]]
	if !strings.Contains(line, item.Key) {
		t.Errorf("the changed line is %q, which is not this setting's", line)
	}

	// The edited file is a config file like any other, so it must parse and serialise back
	// to itself. Without this the guarantee holds only for files nobody has edited yet.
	if got := Parse(after).Bytes(); !bytes.Equal(got, after) {
		g, w := firstDiff(got, after)
		t.Errorf("the edited file no longer round-trips:\n got %q\nwant %q", g, w)
	}
}

// anotherValue derives a legal value that differs from the setting's current one, using only
// what the schema carries — which is what a form has to do, and so is also a check that the
// projection carries enough to edit with.
func anotherValue(item *SchemaItem) (any, bool) {
	switch current := item.Current.(type) {
	case bool:
		return !current, true
	case float64:
		if item.Range != nil {
			if current != item.Range.Min {
				return item.Range.Min, true
			}
			if current != item.Range.Max {
				return item.Range.Max, true
			}
			return nil, false // a range with one point in it
		}
		return current + 1, true
	case string:
		for _, option := range item.Options {
			if option != current {
				return option, true
			}
		}
		if len(item.Options) > 0 {
			return nil, false
		}
		return current + "-edited", true
	default:
		return nil, false
	}
}

// changedLines returns the indices of the lines that differ, and fails the comparison
// outright if the line count moved.
func changedLines(before, after string) []int {
	was, is := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(was) != len(is) {
		return []int{-1, -1} // any length but one, so the caller reports it
	}
	var changed []int
	for i := range was {
		if was[i] != is[i] {
			changed = append(changed, i)
		}
	}
	return changed
}

// TestTheCorpusIsExercisedAtAll guards the harness rather than the parser. Every assertion
// above is inside a loop, and a loop over an empty list passes: a corpus that stopped being
// found, or a projection that stopped emitting settings, would leave this file green while
// proving nothing.
func TestTheCorpusIsExercisedAtAll(t *testing.T) {
	files, settings := 0, 0
	for _, path := range corpusFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, section := range Parse(raw).Schema(filepath.Base(path)).Sections {
			settings += len(section.Settings)
		}
	}
	// The floors sit under what the corpus holds today (14 files, 43 settings) rather than
	// at it: this is here to catch a collapse to nothing, not to be edited every time a
	// fixture is added.
	if files < 10 || settings < 40 {
		t.Errorf("the chain ran over %d files and %d settings, which is too few to be the corpus",
			files, settings)
	}
}
