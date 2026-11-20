package api

import (
	"strings"
	"testing"
)

func TestCheckCfgLines(t *testing.T) {
	const current = "## Settings\r\n[General]\r\n\r\n# A comment\r\nEnabled = true\r\n"
	cases := []struct {
		name    string
		next    string
		wantErr string
	}{
		{"unchanged", current, ""},
		{"a new setting", current + "Speed = 2\r\n", ""},
		{"an unclosed section", current + "[Other\n", "line 6: the section header has no closing ]"},
		{"a line that is no setting", current + "Speed 2\n", "line 6: expected a setting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCfgLines([]byte(current), []byte(tc.next))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckCfgLinesLeavesAModsOwnDialectAlone(t *testing.T) {
	if err := checkCfgLines([]byte("a: 1\n"), []byte("a: 2\n")); err != nil {
		t.Fatalf("err = %v, want none for a file that was never BepInEx syntax", err)
	}
}

func TestMatchLineEndings(t *testing.T) {
	cases := []struct {
		name, current, next, want string
	}{
		{"an LF file stays LF", "a = 1\n", "a = 2\nb = 3\n", "a = 2\nb = 3\n"},
		{"a CRLF file keeps CRLF", "a = 1\r\n", "a = 2\nb = 3\n", "a = 2\r\nb = 3\r\n"},
		{"mixed input is not doubled", "a = 1\r\n", "a = 2\r\nb = 3\n", "a = 2\r\nb = 3\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(matchLineEndings([]byte(tc.current), []byte(tc.next))); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
