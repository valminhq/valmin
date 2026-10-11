package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
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
	t.Parallel()
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
	// The two honesty flags. Both are measured facts about the measurements, and a UI
	// that cannot see them presents a probe result as an enumeration.
	if got.PresetsComplete {
		t.Error("presets_complete is true; 03 §1.3.1 says completeness is not proven and must not be claimed")
	}
	if len(got.Modifiers) != 5 || len(got.ModifierKeys) != 5 || len(got.WorldKeys) != 5 {
		t.Errorf("modifiers = %v, keys = %v, world keys = %v, want five of each",
			got.Modifiers, got.ModifierKeys, got.WorldKeys)
	}
	if got.Saves.SaveIntervalSeconds != 1800 || got.Saves.Backups != 4 {
		t.Errorf("save defaults = %+v, want the measured 1800 s / 4", got.Saves)
	}
	// 03 §1.4 rule 5 is data, not copy: the list is what the screens render, and it is empty
	// now that the modded-client join is measured (Q6).
	if len(got.CrossplayUntested) != 0 {
		t.Errorf("crossplay_untested = %v, want no unmeasured combination", got.CrossplayUntested)
	}
	if got.MinPasswordLength != instance.MinPasswordLength {
		t.Errorf("min_password_length = %d", got.MinPasswordLength)
	}
	if got.MinMemoryLimitMB != instance.MinMemoryLimitMB {
		t.Errorf("min_memory_limit_mb = %d", got.MinMemoryLimitMB)
	}
}

// TestGameOptionsIsServedToEverySignedInCaller asserts that the launch vocabulary reaches every
// signed-in account, grant or not, since a member's settings screen renders it, and that a
// request without a session is still unauthenticated.
func TestGameOptionsIsServedToEverySignedInCaller(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		admin bool
		setup func(t *testing.T, db *store.DB)
		want  int
	}{
		{name: "admin", admin: true, want: http.StatusOK},
		{name: "viewer member", want: http.StatusOK},
		{name: "member holding instance.settings", setup: func(t *testing.T, db *store.DB) {
			grantOperatorWith(t, db, "instance.settings")
		}, want: http.StatusOK},
		{name: "member without a grant", setup: func(t *testing.T, db *store.DB) {
			seed(t, db, `DELETE FROM instance_grants WHERE user_id = 'u-member'`)
		}, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, admin, member := world(t)
			if tc.setup != nil {
				tc.setup(t, db)
			}
			u := member
			if tc.admin {
				u = admin
			}
			rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/game/options", http.NoBody))
			if rec.Code != tc.want {
				t.Fatalf("GET /game/options = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			if got := readOptions(t, rec); got.Build != instance.GameBuild {
				t.Errorf("build = %q, want %q", got.Build, instance.GameBuild)
			}
		})
	}

	t.Run("no session", func(t *testing.T) {
		rt, _, _, _ := world(t)
		rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/game/options", http.NoBody))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /game/options without a session = %d, want 401", rec.Code)
		}
	})
}

// TestModifierVocabularyIsWellFormed asserts that every axis has exactly one Normal position
// and no repeated value, and that no world key shares a name with an axis.
func TestModifierVocabularyIsWellFormed(t *testing.T) {
	t.Parallel()
	for _, m := range instance.Modifiers {
		seen := map[string]bool{}
		for _, o := range m.Values {
			if seen[o.Value] {
				t.Errorf("%s repeats value %q", m.Key, o.Value)
			}
			seen[o.Value] = true
		}
		if !seen[""] {
			t.Errorf("%s has no Normal position", m.Key)
		}
		if instance.IsWorldKey(m.Key) {
			t.Errorf("axis %s is also a world key", m.Key)
		}
	}
}
