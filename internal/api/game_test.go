package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
)

func readOptions(t *testing.T, rec *httptest.ResponseRecorder) gameOptions {
	t.Helper()
	var got gameOptions
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return got
}

// TestGameOptionsCarriesTheMeasurementsAndTheirLimits is F2 made possible: the SPA renders a
// preset select without knowing what a preset is, because the measured vocabulary of
// 03 §1.3 is served rather than hardcoded in the frontend.
func TestGameOptionsCarriesTheMeasurementsAndTheirLimits(t *testing.T) {
	rt, _, admin, _ := world(t)

	got := readOptions(t, as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/game/options", http.NoBody)))

	if got.Build != "21981559" {
		t.Errorf("build = %q; the values are stamped with the build they were measured against", got.Build)
	}
	for _, want := range []string{"normal", "hammer", "default"} {
		if !slices.Contains(got.Presets, want) {
			t.Errorf("preset %q is missing: %v", want, got.Presets)
		}
	}
	// `↯` The two honesty flags. Both are measured facts about the measurements, and a UI
	// that cannot see them presents a probe result as an enumeration.
	if got.PresetsComplete {
		t.Error("presets_complete is true; 03 §1.3.1 says completeness is not proven and must not be claimed")
	}
	if got.ModifierValues {
		t.Error("modifier_values_measured is true; 03 §4.2 says the stored form is not proven to be the CLI grammar")
	}
	if got.Saves.SaveIntervalSeconds != 1800 || got.Saves.Backups != 4 {
		t.Errorf("save defaults = %+v, want the measured 1800 s / 4", got.Saves)
	}
	if len(got.CrossplayUntested) != 2 {
		t.Errorf("crossplay_untested = %v, want 03 §1.4's two combinations", got.CrossplayUntested)
	}
	if got.MinPasswordLength != instance.MinPasswordLength {
		t.Errorf("min_password_length = %d", got.MinPasswordLength)
	}
}

// TestGameOptionsIsAdminOnly: it fills in a form only an admin can submit (09 §3.3 makes
// instance.create never grantable), and a member gets the same not_found every invisible
// resource gets (D2).
func TestGameOptionsIsAdminOnly(t *testing.T) {
	rt, _, _, member := world(t)

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/game/options", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("member GET /game/options = %d, want 404", rec.Code)
	}
}

// TestNoGuessedModifierValuesExist is E8 as a guard. 03 §4.2 measured the `.fwl`'s stored
// form — `combat_default:deathpenalty_default:…` — and says in the same breath that it is
// not proven identical to the command-line grammar. Turning it into a dropdown would be
// inference presented to the operator as a measurement, and the server refusing to start is
// how they would find out.
func TestNoGuessedModifierValuesExist(t *testing.T) {
	if instance.ModifierValuesMeasured {
		t.Fatal("modifier values are marked measured; 03 §4.2 does not measure them")
	}
	if len(instance.ModifierKeys) != 5 {
		t.Errorf("modifier keys = %v, want 03 §1.3's five axes", instance.ModifierKeys)
	}
}
