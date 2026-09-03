package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusFiles lists every .cfg under testdata/, so a test can assert over the whole corpus
// without naming its files.
func corpusFiles(t testing.TB) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".cfg" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the corpus: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the corpus is empty")
	}
	return found
}

// TestCorpusKeepsItsBytes asserts each fixture still carries the byte property it exists to
// exercise. A checkout or formatter that normalised one would leave the harness proving
// nothing.
func TestCorpusKeepsItsBytes(t *testing.T) {
	tests := []struct {
		file string
		want string
		ok   func([]byte) bool
	}{
		{
			"edge/bom.cfg", "a UTF-8 BOM",
			func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) },
		},
		{
			"edge/crlf.cfg", "CRLF on every line",
			func(b []byte) bool { return !bytes.Contains(dropCRLF(b), []byte("\n")) },
		},
		{
			"edge/no-trailing-newline.cfg", "no final line ending",
			func(b []byte) bool { return len(b) > 0 && b[len(b)-1] != '\n' },
		},
		{
			"edge/trailing-whitespace.cfg", "trailing whitespace before a line ending",
			func(b []byte) bool { return bytes.Contains(b, []byte(" \n")) },
		},
		{
			"plugin/BepInEx.cfg", "both CRLF and bare-LF line endings",
			func(b []byte) bool {
				return bytes.Contains(b, []byte("\r\n")) && bytes.Contains(dropCRLF(b), []byte("\n"))
			},
		},
		{
			"plugin/BepInEx.cfg", "a description line that is `## ` with a trailing space",
			func(b []byte) bool { return bytes.Contains(b, []byte("\n## \r\n")) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.file+" has "+tt.want, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			if !tt.ok(raw) {
				t.Errorf("%s: expected %s; something normalised the fixture", tt.file, tt.want)
			}
		})
	}
}

// dropCRLF removes complete CRLF pairs, leaving the line endings that are a bare LF.
func dropCRLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), nil)
}

// TestCorpusIsReadableWithoutAnEnvironmentVariable asserts the corpus is committed rather than
// downloaded, unlike the mod archives ADR-105 reaches through VALMIN_MOD_CORPUS.
func TestCorpusIsReadableWithoutAnEnvironmentVariable(t *testing.T) {
	t.Setenv("VALMIN_MOD_CORPUS", "")
	for _, path := range corpusFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if len(raw) == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

// TestCorpusCoversEverySettingType asserts the corpus names every type and metadata form
// 03 §9 lists, so no widget goes unexercised.
func TestCorpusCoversEverySettingType(t *testing.T) {
	var all strings.Builder
	for _, path := range corpusFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	corpus := all.String()

	for _, want := range []string{
		"# Setting type: Boolean",
		"# Setting type: Int32",
		"# Setting type: Single",
		"# Setting type: Double",
		"# Setting type: String",
		"# Setting type: KeyboardShortcut",
		"# Setting type: Color",
	} {
		if !strings.Contains(corpus, want) {
			t.Errorf("no fixture carries %q", want)
		}
	}
	for _, want := range []string{
		"# Acceptable value range: From ",
		"# Acceptable values: ",
		"# Multiple values can be set",
	} {
		if !strings.Contains(corpus, want) {
			t.Errorf("no fixture carries the metadata form %q", want)
		}
	}
}
