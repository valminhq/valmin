package instance

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestEyeLinesDecodeOrAreIgnored asserts each fixture line is claimed by the right kind, and
// that only version 1 event objects decode.
func TestEyeLinesDecodeOrAreIgnored(t *testing.T) {
	capture, err := os.ReadFile("testdata/valmin-eye.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(capture)), "\n")
	tests := []struct {
		kind   EventKind
		event  string
		player string
	}{
		{EventPluginLoading, "", ""},
		{"", "", ""},
		{EventEye, "join", "Alex"},
		{EventEye, "boss_fight_end", ""},
		{EventEye, "boss_first_kill", ""},
		{EventEye, "", ""},
		{EventEye, "join", "World save writing finished"},
		{EventLoaderError, "", ""},
		{EventEye, "leave", "Alex"},
	}
	if len(lines) != len(tests) {
		t.Fatalf("fixture has %d lines, want %d", len(lines), len(tests))
	}
	for i, tt := range tests {
		ev, matched := DefaultPatterns.Match(lines[i])
		if ev.Kind != tt.kind || matched != (tt.kind != "") {
			t.Errorf("Match(%q) = %q, want %q", lines[i], ev.Kind, tt.kind)
			continue
		}
		if ev.Kind != EventEye {
			continue
		}
		got, ok := parseEye(ev.Groups[1], time.Time{})
		if ok != (tt.event != "") || got.Name != tt.event {
			t.Errorf("parseEye(%q) = %q, %v, want %q", ev.Groups[1], got.Name, ok, tt.event)
			continue
		}
		if player, _ := got.Fields["player"].(string); player != tt.player {
			t.Errorf("parseEye(%q) player = %q, want %q", ev.Groups[1], player, tt.player)
		}
	}
}

func TestParseEyeRejectsMalformedObjects(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"event":"join"`,
		`{"v":1}`,
		`{"event":"join"}`,
		`{"v":"1","event":"join"}`,
		`{"v":1.0,"event":"join"}`,
	} {
		if got, ok := parseEye(raw, time.Time{}); ok {
			t.Errorf("parseEye(%q) = %+v, want rejected", raw, got)
		}
	}
}

// TestTheReaderAnnouncesOnlyEyeEventsAfterItOpened asserts a line re-read from before the
// stream opened is not announced again, and a newer one is.
func TestTheReaderAnnouncesOnlyEyeEventsAfterItOpened(t *testing.T) {
	r := newReader(DefaultPatterns)
	var got []EyeEvent
	r.onEye = func(ev EyeEvent) { got = append(got, ev) }
	opened := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	r.opened(opened)

	text := `[Info   : ValminEye] {"v":1,"event":"join","player":"Alex","id":"Steam_1"}`
	r.append(Line{Stream: StreamStdout, TS: opened.Add(-time.Minute), Text: text})
	r.append(Line{Stream: StreamStdout, TS: opened.Add(time.Second), Text: text})

	if len(got) != 1 || got[0].Name != "join" || !got[0].TS.Equal(opened.Add(time.Second)) {
		t.Errorf("announced %+v, want only the join after the open", got)
	}
}
