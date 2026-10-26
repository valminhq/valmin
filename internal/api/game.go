package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
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
	// Branches is every Steam branch the last update check saw, public first. Empty before
	// the first check.
	Branches []instance.SteamBranch `json:"branches"`
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
	branches, err := h.observedBranches(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		Branches:          branches,
	})
}

// observedBranches is the branch list of the last successful update check.
func (h *Instances) observedBranches(ctx context.Context) ([]instance.SteamBranch, error) {
	var observed instance.PublicBuild
	if _, err := h.DB.KVGet(ctx, instance.PublicBuildKey, &observed); err != nil {
		return nil, fmt.Errorf("read the observed builds: %w", err)
	}
	if observed.Branches == nil {
		return []instance.SteamBranch{}, nil
	}
	return observed.Branches, nil
}

// validateBranch checks a requested branch and password. Without a password the branch must be
// public or one the last update check saw; with one it is a password branch, which Steam does
// not list to an anonymous login. The error it returns is an internal one, ready to write.
func (h *Instances) validateBranch(ctx context.Context, val *apierr.Validation, branch, password string) error {
	if branch == "" || branch == instance.PublicBranch {
		if password != "" {
			val.Add("game_branch_password", apierr.FieldInvalid, "The public branch takes no password.")
		}
		return nil
	}
	if err := instance.ValidBranchName(branch); err != nil {
		val.Add("game_branch", apierr.FieldInvalid, sentence(err))
		return nil
	}
	if password != "" {
		if err := instance.ValidBranchPassword(password); err != nil {
			val.Add("game_branch_password", apierr.FieldInvalid, sentence(err))
		}
		return nil
	}
	branches, err := h.observedBranches(ctx)
	if err != nil {
		return apierr.New(errcode.Internal).Wrap(err)
	}
	for _, b := range branches {
		if b.Name == branch {
			return nil
		}
	}
	val.Add("game_branch", apierr.FieldInvalid,
		"Steam does not list this branch. A password branch needs its password.")
	return nil
}

// sentence turns a validator's error into a field message.
func sentence(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// sealBranchPassword encrypts a branch password for instance id, or returns "" for none.
func (h *Instances) sealBranchPassword(id, password string) (string, error) {
	if password == "" {
		return "", nil
	}
	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeBranchPassword, crypto.BranchPasswordLocation(id), []byte(password))
	if err != nil {
		return "", fmt.Errorf("encrypt branch password: %w", err)
	}
	return envelope, nil
}

// patchNotes is GET /game/patch-notes: the game's recent patch posts from Steam, newest first.
// A feed Steam cannot be reached for answers 503 and the page falls back to a link.
func (h *Instances) patchNotes(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	notes, err := h.PatchNotes.Notes(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Unavailable).Msg("Steam's news could not be reached.").Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, map[string]any{"notes": notes})
}
