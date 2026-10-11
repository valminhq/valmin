package sharecode

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance/control"
)

const maxBytes = 8 << 20

func fixture() *Template {
	cpu := 1.5
	return &Template{
		Name: "Vanilla QoL & friends",
		Launch: control.ManifestLaunch{
			ServerName: "Viking Friends", WorldName: "Midgard", Public: true, Crossplay: true,
			Preset: "hard", Modifiers: map[string]string{"combat": "hard", "raids": "none"},
			ExtraArgs: "-saveinterval 600", MemLimitMB: 8192, CPULimit: &cpu,
			BackupKeepCold: 10, BackupKeepHot: 5, BackupOnRestart: true,
		},
		Mods: []control.ManifestMod{
			{
				FullName: "ArgusMagnus-ServersideQoL_AutoDoors",
				Version:  "2.2.0",
				Source:   "thunderstore",
				Side:     "server_only",
			},
			{FullName: "Smoothbrain-Sailing", Version: "1.1.9", Source: "hexium"},
			{FullName: "Smoothbrain-Mining", Version: "1.1.7", Source: "hexium", Side: "client_required"},
			{FullName: "Marf-FuelEternal", Version: "1.2.1-beta.2", Source: "thunderstore"},
		},
		Configs: []control.ManifestConfig{
			{
				File:    "argusmagnus.ServersideQoL.cfg",
				Content: "[B - Doors]\nAutoCloseMinPlayerDistance = 8\n\n[Logging.Console]\nEnabled = a = b\n",
			},
			{File: "Marf.FuelEternal.cfg", Content: "TopLevel = 1\n[General]\nConfigLocked = true\n"},
		},
	}
}

// TestRoundTrip asserts a template survives Encode then Decode.
func TestRoundTrip(t *testing.T) {
	cases := map[string]*Template{
		"everything set": fixture(),
		"nothing set":    {},
		"mods only": {Mods: []control.ManifestMod{
			{FullName: "A-B", Version: "1.0.0", Source: "thunderstore"},
		}},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			code, err := Encode(want)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(code, maxBytes)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestDecodeReadsTheCommittedCode asserts a code written by this format version keeps decoding.
func TestDecodeReadsTheCommittedCode(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.code")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(string(raw), maxBytes)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := fixture(); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded =\n%+v\nwant\n%+v", got, want)
	}
}

// TestDecodeIgnoresWhitespace asserts a code wrapped across lines by a chat client still reads.
func TestDecodeIgnoresWhitespace(t *testing.T) {
	code, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	wrapped := "  " + code[:10] + "\n" + code[10:20] + " \t" + code[20:] + "\r\n"
	if _, err := Decode(wrapped, maxBytes); err != nil {
		t.Errorf("Decode of a wrapped code: %v", err)
	}
}

// TestDecodeRejects asserts each malformed code is refused with the matching error.
func TestDecodeRejects(t *testing.T) {
	deflated := func(payload string) string {
		var buf bytes.Buffer
		w, _ := flate.NewWriter(&buf, flate.BestCompression)
		_, _ = w.Write([]byte(payload))
		_ = w.Close()
		return Prefix + base64.RawURLEncoding.EncodeToString(buf.Bytes())
	}
	good, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		code string
		want error
	}{
		{"empty", "", ErrNotACode},
		{"another prefix", "hello:abc", ErrNotACode},
		{"a later version", "valmin2:abc", ErrNewer},
		{"bad base64", Prefix + "!!!", ErrDamaged},
		{"truncated", good[:len(good)-8], ErrDamaged},
		{"not deflate", Prefix + base64.RawURLEncoding.EncodeToString([]byte("plain text")), ErrDamaged},
		{"too large", deflated("n=x\n" + strings.Repeat("A-B 1.0.0\n", 1000)), ErrTooLarge},
		{"no newline", deflated("n=x"), errBadPayload},
		{"unknown header field", deflated("zz=1\n"), errBadPayload},
		{"repeated header field", deflated("n=a&n=b\n"), errBadPayload},
		{"flag that is not 1", deflated("p=yes\n"), errBadPayload},
		{"cpu that is not finite", deflated("cpu=NaN\n"), errBadPayload},
		{"modifier without a value", deflated("m=combat\n"), errBadPayload},
		{"mod line with one field", deflated("n=x\nA-B\n"), errBadPayload},
		{"mod line with a double space", deflated("n=x\nA-B  1.0.0\n"), errBadPayload},
		{"unknown side", deflated("n=x\nA-B 1.0.0 q\n"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := int64(maxBytes)
			if errors.Is(tc.want, ErrTooLarge) {
				limit = 100
			}
			_, err := Decode(tc.code, limit)
			if err == nil {
				t.Fatal("Decode accepted it")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestEncodeRejects asserts a template the format cannot hold is refused rather than written.
func TestEncodeRejects(t *testing.T) {
	cases := map[string]*Template{
		"mod name with a space":    {Mods: []control.ManifestMod{{FullName: "A B", Version: "1.0.0"}}},
		"mod name starting with #": {Mods: []control.ManifestMod{{FullName: "#A", Version: "1.0.0"}}},
		"mod without a version":    {Mods: []control.ManifestMod{{FullName: "A-B"}}},
		"unknown side":             {Mods: []control.ManifestMod{{FullName: "A-B", Version: "1.0.0", Side: "both"}}},
		"config line starting #":   {Configs: []control.ManifestConfig{{File: "a.cfg", Content: "# comment\n"}}},
		"modifier with a colon":    {Launch: control.ManifestLaunch{Modifiers: map[string]string{"a:b": "c"}}},
	}
	for name, tmpl := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Encode(tmpl); err == nil {
				t.Error("Encode accepted it")
			}
		})
	}
}

// FuzzDecode asserts Decode never panics and that whatever it accepts encodes back to the same
// template.
func FuzzDecode(f *testing.F) {
	if raw, err := os.ReadFile("testdata/v1.code"); err == nil {
		f.Add(string(raw))
	}
	f.Add(Prefix)
	f.Add("valmin1:AAAA")
	f.Fuzz(func(t *testing.T, code string) {
		got, err := Decode(code, 1<<16)
		if err != nil {
			return
		}
		again, err := Encode(got)
		if err != nil {
			t.Fatalf("Encode of a decoded template: %v", err)
		}
		back, err := Decode(again, 1<<20)
		if err != nil {
			t.Fatalf("Decode of a re-encoded template: %v", err)
		}
		if !reflect.DeepEqual(back, got) {
			t.Fatalf("re-encoded template differs:\n%+v\n%+v", back, got)
		}
	})
}
