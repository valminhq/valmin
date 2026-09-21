package source

import (
	"encoding/json"
	"testing"
)

// TestByName asserts every wire name resolves to its constant, and that anything else is
// reported as unrecognised rather than resolving to a registry.
func TestByName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Source
		ok    bool
	}{
		{"thunderstore", "thunderstore", Thunderstore, true},
		{"hexium", "hexium", Hexium, true},
		{"empty", "", Source{}, false},
		{"wrong case", "Thunderstore", Source{}, false},
		{"unknown", "nexus", Source{}, false},
		{"whitespace", " hexium", Source{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ByName(tt.input)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ByName(%q) = (%v, %v), want (%v, %v)", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestAllRoundTrips asserts every registry All reports is resolvable by its own wire name,
// so a registry can never be added to the set without a name that parses back.
func TestAllRoundTrips(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("All returned no registries")
	}
	for _, s := range all {
		got, ok := ByName(s.String())
		if !ok || got != s {
			t.Errorf("%q did not round-trip: got (%v, %v)", s, got, ok)
		}
	}
}

// TestAllIsACopy asserts a caller cannot reorder or overwrite the registry set.
func TestAllIsACopy(t *testing.T) {
	first := All()
	first[0] = Source{"tampered"}
	if All()[0] != Thunderstore {
		t.Fatalf("All()[0] = %v, want thunderstore", All()[0])
	}
}

// TestMarshalJSON asserts the wire form is the name, since the DTOs embed it directly.
func TestMarshalJSON(t *testing.T) {
	b, err := json.Marshal(struct {
		Source Source `json:"source"`
	}{Hexium})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"source":"hexium"}` {
		t.Fatalf("got %s", b)
	}
}

// TestJSONRoundTrip is what keeps a Source from decoding to nothing in silence: the type has
// an unexported field, so without UnmarshalJSON every decode would yield the zero value and
// no error, and a record read back off disk would name no registry.
func TestJSONRoundTrip(t *testing.T) {
	type row struct {
		Source Source `json:"source"`
	}
	for _, want := range All() {
		b, err := json.Marshal(row{want})
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		var got row
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got.Source != want {
			t.Errorf("%s round-tripped to %v, want %v", b, got.Source, want)
		}
	}
}

// TestUnmarshalRejectsAnUnknownRegistry asserts a name no build recognises is an error, while
// an absent one is the zero value a record written before the field existed reads as.
func TestUnmarshalRejectsAnUnknownRegistry(t *testing.T) {
	var s Source
	if err := json.Unmarshal([]byte(`"nexus"`), &s); err == nil {
		t.Error("an unknown registry decoded without an error")
	}
	for _, empty := range []string{`""`, `null`} {
		var zero Source
		if err := json.Unmarshal([]byte(empty), &zero); err != nil {
			t.Errorf("%s: %v", empty, err)
		}
		if zero != (Source{}) {
			t.Errorf("%s decoded to %v, want the zero Source", empty, zero)
		}
	}
}
