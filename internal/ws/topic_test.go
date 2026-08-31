package ws

import (
	"testing"

	"github.com/valminhq/valmin/internal/authz"
)

const idA = "01920000-0000-7000-8000-00000000000a"

func TestParseCoversTheWholeRegistryAndNothingElse(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		kind  Kind
		class Class
		act   authz.Action
	}{
		{"instance." + idA + ".console", true, KindConsole, Lossy, authz.ConsoleRead},
		{"instance." + idA + ".stats", true, KindStats, Lossy, authz.StatsRead},
		{"instance." + idA + ".state", true, KindState, Lossless, authz.InstanceView},
		{"job." + idA, true, KindJob, Lossless, authz.Action{}},

		// `↯` ADR-040: no wildcards. A standing subscription would have to be
		// re-authorized every time a grant changes or an instance is created, which turns
		// one authorization decision into a permanent one.
		{"instance.*.state", false, 0, 0, authz.Action{}},
		{"instance.*.console", false, 0, 0, authz.Action{}},
		{"job.*", false, 0, 0, authz.Action{}},
		{"*", false, 0, 0, authz.Action{}},

		{"instance." + idA, false, 0, 0, authz.Action{}},
		{"instance." + idA + ".logs", false, 0, 0, authz.Action{}},
		{"instance." + idA + ".console.extra", false, 0, 0, authz.Action{}},
		{"job." + idA + ".progress", false, 0, 0, authz.Action{}},
		{"instance..console", false, 0, 0, authz.Action{}},
		{"instance.../../etc/passwd.console", false, 0, 0, authz.Action{}},
		{"instance.' OR 1=1 --.console", false, 0, 0, authz.Action{}},
		{"", false, 0, 0, authz.Action{}},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.in)
		if ok != tc.ok {
			t.Errorf("Parse(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind() != tc.kind {
			t.Errorf("Parse(%q).Kind() = %v, want %v", tc.in, got.Kind(), tc.kind)
		}
		if got.Class() != tc.class {
			t.Errorf("Parse(%q).Class() = %v, want %v", tc.in, got.Class(), tc.class)
		}
		if got.Action() != tc.act {
			t.Errorf("Parse(%q).Action() = %v, want %v", tc.in, got.Action(), tc.act)
		}
		if got.String() != tc.in {
			t.Errorf("Parse(%q).String() = %q; the wire form must round-trip", tc.in, got.String())
		}
	}
}

// TestConsoleAndStatsAreLossyStateAndJobAreNot is ADR-039 stated as a test, because the
// class is what decides whether a full queue drops a message or closes the connection —
// getting it backwards for `state` shows a running server that is not.
func TestConsoleAndStatsAreLossyStateAndJobAreNot(t *testing.T) {
	for _, t2 := range []Topic{ConsoleTopic(idA), StatsTopic(idA)} {
		if t2.Class() != Lossy {
			t.Errorf("%s is not lossy", t2)
		}
	}
	for _, t2 := range []Topic{StateTopic(idA), JobTopic(idA)} {
		if t2.Class() != Lossless {
			t.Errorf("%s is not lossless", t2)
		}
	}
}
