package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
)

// gameOptions is what the create wizard needs to render itself without knowing what a
// preset is (F2, 02 §2.1). Every value is measured; the two `_verified` flags exist because
// two of them are measured *and* known to be incomplete, and a UI that cannot tell the
// difference will present a guess as a fact.
type gameOptions struct {
	Build             string                `json:"build"`
	Presets           []string              `json:"presets"`
	PresetsComplete   bool                  `json:"presets_complete"`
	ModifierKeys      []string              `json:"modifier_keys"`
	ModifierValues    bool                  `json:"modifier_values_measured"`
	Saves             instance.SaveDefaults `json:"save_defaults"`
	CrossplayUntested []string              `json:"crossplay_untested"`
	MinPasswordLength int                   `json:"min_password_length"`
}

// options is GET /game/options.
//
// An addition to 04 §3's surface, and an additive one (11 §1). It exists because F2
// forbids the alternative: a preset list hardcoded in the SPA is Valheim knowledge in the
// frontend, and it would be a second copy of 03 §1.3's measurements with no build stamp and
// nothing to keep it in step on 9 September.
//
// Gated on instance.create, which 09 §3.3 makes admin-only and never grantable — the same
// gate as the endpoint this data is used to fill in. It advertises the measured presets and
// does not reject an unlisted one: 03 §1.3.1 states the enumeration is not proven
// complete, so refusing a value the game might accept would be the panel inventing a limit
// the game does not have.
func (h *Instances) options(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	JSON(w, r, http.StatusOK, gameOptions{
		Build:             instance.GameBuild,
		Presets:           instance.Presets,
		PresetsComplete:   instance.PresetsComplete,
		ModifierKeys:      instance.ModifierKeys,
		ModifierValues:    instance.ModifierValuesMeasured,
		Saves:             instance.Saves,
		CrossplayUntested: instance.CrossplayUntested,
		MinPasswordLength: instance.MinPasswordLength,
	})
}
