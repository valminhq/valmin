package api

import (
	"net/http"

	"github.com/valminhq/valmin/internal/instance"
)

// gameOptions is what the create wizard and the settings screen need to render themselves
// without knowing what a preset is. Every value is measured, and presets_complete marks the
// preset list as not exhaustive. modifier_keys and modifier_values_measured predate modifiers
// and stay for existing clients.
type gameOptions struct {
	Build             string                 `json:"build"`
	Presets           []string               `json:"presets"`
	PresetsComplete   bool                   `json:"presets_complete"`
	ModifierKeys      []string               `json:"modifier_keys"`
	ModifierValues    bool                   `json:"modifier_values_measured"`
	Modifiers         []instance.Modifier    `json:"modifiers"`
	WorldKeys         []instance.Option      `json:"world_keys"`
	Saves             instance.SaveDefaults  `json:"save_defaults"`
	CrossplayUntested []string               `json:"crossplay_untested"`
	MinPasswordLength int                    `json:"min_password_length"`
	MinMemoryLimitMB  int                    `json:"min_memory_limit_mb"`
	CustomSeed        instance.CustomSeedMod `json:"custom_seed"`
}

// options is GET /game/options, an additive addition to 04 §3's surface (11 §1). F2 forbids a
// preset list hardcoded in the SPA, which would be Valheim knowledge in the frontend and a
// second, unstamped copy of 03 §1.3's measurements.
//
// Served to any signed-in caller: it is build constants, resolves no panel resource, and every
// member's settings screen renders it. It is exempt from the Can() call-site test on those
// grounds (authz_callsite_test.go). It advertises the measured presets and does not reject an
// unlisted one, since 03 §1.3.1's enumeration is not proven complete.
func (h *Instances) options(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	keys := make([]string, 0, len(instance.Modifiers))
	for _, m := range instance.Modifiers {
		keys = append(keys, m.Key)
	}
	JSON(w, r, http.StatusOK, gameOptions{
		Build:             instance.GameBuild,
		Presets:           instance.Presets,
		PresetsComplete:   instance.PresetsComplete,
		ModifierKeys:      keys,
		ModifierValues:    true,
		Modifiers:         instance.Modifiers,
		WorldKeys:         instance.WorldKeys,
		Saves:             instance.Saves,
		CrossplayUntested: instance.CrossplayUntested,
		MinPasswordLength: instance.MinPasswordLength,
		MinMemoryLimitMB:  instance.MinMemoryLimitMB,
		CustomSeed:        instance.CustomSeed,
	})
}
