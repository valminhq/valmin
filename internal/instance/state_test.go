package instance

import (
	"sort"
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
)

// allStates is every value 12 §2.1 names, independent of edgeList — so the exhaustiveness
// check below cannot pass by construction.
var allStates = []State{
	StateCreated, StateProvisioning, StateStopped, StateStarting, StateRunning, StateStopping,
	StateBackingUp, StateRestoring, StateUpdating, StateDeleting, StateError,
}

// documentedEdges transcribes 12 §2.2 independently of state.go's edgeList, so a bug that
// corrupts one copy cannot also corrupt the test asserting against it. The one addition —
// stopping -> starting — is noted at its own edge in state.go; restart has no row of its
// own in 12 §2.2 but is scoped `running`, entered `stopping→starting` by 12 §3.1.
var documentedEdges = map[[2]State]bool{
	{StateCreated, StateProvisioning}: true,
	{StateProvisioning, StateStopped}: true,
	{StateProvisioning, StateError}:   true,
	{StateStopped, StateStarting}:     true,
	{StateStarting, StateRunning}:     true,
	{StateStarting, StateError}:       true,
	{StateRunning, StateStopping}:     true,
	{StateStopping, StateStarting}:    true,
	{StateStopping, StateStopped}:     true,
	{StateStopping, StateError}:       true,
	{StateRunning, StateStopped}:      true,
	{StateRunning, StateError}:        true,
	{StateStopped, StateRunning}:      true,
	{StateStopped, StateBackingUp}:    true,
	{StateStopped, StateRestoring}:    true,
	{StateStopped, StateUpdating}:     true,
	{StateBackingUp, StateStopped}:    true,
	{StateRestoring, StateStopped}:    true,
	{StateRestoring, StateError}:      true,
	{StateUpdating, StateStopped}:     true,
	{StateUpdating, StateError}:       true,
	{StateStopped, StateDeleting}:     true,
	{StateError, StateDeleting}:       true,
	{StateError, StateStopped}:        true,
	{StateError, StateRunning}:        true,
}

// TestEveryDocumentedTransitionIsValid is half of 05 M1's acceptance test: every row of
// 12 §2.2 (plus the restart edge noted in state.go) is exercised.
func TestEveryDocumentedTransitionIsValid(t *testing.T) {
	for pair := range documentedEdges {
		if !Valid(pair[0], pair[1]) {
			t.Errorf("Valid(%s, %s) = false, want true", pair[0], pair[1])
		}
	}
}

// TestNoUndocumentedTransitionIsValid is the other half: every pair NOT in 12 §2.2 is
// rejected — exhaustive over all 11×11 state pairs, not spot-checked.
func TestNoUndocumentedTransitionIsValid(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			want := documentedEdges[[2]State{from, to}]
			if got := Valid(from, to); got != want {
				t.Errorf("Valid(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestDeletingHasNoOutgoingEdges(t *testing.T) {
	for _, to := range allStates {
		if Valid(StateDeleting, to) {
			t.Errorf("Valid(deleting, %s) = true, want false — deleting only ever ends with the row gone", to)
		}
	}
}

// TestAllowedFromMatchesJobRegisterRequirements cross-checks AllowedFrom against 12 §3.1's
// own "Requires" column, kind by kind.
func TestAllowedFromMatchesJobRegisterRequirements(t *testing.T) {
	for _, tc := range []struct {
		kind jobs.Kind
		want []State
	}{
		{jobs.KindProvision, []State{StateCreated}},
		{jobs.KindStart, []State{StateStopped}},
		{jobs.KindStop, []State{StateRunning}},
		{jobs.KindRestart, []State{StateRunning}},
		{jobs.KindDelete, []State{StateError, StateStopped}},
	} {
		got := AllowedFrom(tc.kind)
		sort.Slice(tc.want, func(i, j int) bool { return tc.want[i] < tc.want[j] })
		if !equalStates(got, tc.want) {
			t.Errorf("AllowedFrom(%s) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// TestStartFromErrorIsRejected is 05 M1's own acceptance wording, proven at the guard this
// package owns — the HTTP endpoint that calls it lands in WP-14, once `start` itself does.
func TestStartFromErrorIsRejected(t *testing.T) {
	if Valid(StateError, StateStarting) {
		t.Fatal("start must never be claimable from error (12 §2.4)")
	}
	allowed := AllowedFrom(jobs.KindStart)
	if len(allowed) != 1 || allowed[0] != StateStopped {
		t.Errorf("AllowedFrom(start) = %v, want [stopped] — the 409's allowed_states", allowed)
	}
}

func equalStates(a, b []State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
