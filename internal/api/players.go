package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// playerListView is the body of all three lists, in both directions. One shape, because the
// three files differ only in name and in what the game does with them (03 §4).
type playerListView struct {
	IDs []string `json:"ids"`
}

// listRoutes wires the admin, ban and permit lists. The API sketch draws only
// `PUT .../admins`, but all three files are equally editable — a ban list nobody can write
// is not a ban list — so the missing two PUTs are read as an abbreviation in that sketch
// rather than as a decision.
func (h *Instances) listRoutes(rt *Router) {
	for path, list := range map[string]instance.PlayerList{
		"admins":    instance.AdminList,
		"bans":      instance.BannedList,
		"permitted": instance.PermittedList,
	} {
		rt.Handle("GET /api/v1/instances/{id}/"+path, h.readPlayerList(list))
		rt.Handle("PUT /api/v1/instances/{id}/"+path, h.writePlayerList(list))
	}
}

// listETag is 11 §1.1's ETag: the SHA-256 of the bytes on disk. An absent file and an empty
// file hash the same, which is correct — the game treats them identically, so a client that
// GETs a list before one exists can still PUT against the answer it was given.
func listETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// readPlayerList is GET /instances/{id}/{admins,bans,permitted}.
//
// Gated on players.manage even though it only reads. 09 §3.1 gives `viewer` no
// players-shaped action at all, and the never-grantable rule in 09 §3.3 means an action
// cannot be invented here to fill the gap — so read and write share the operator capability
// the registry actually has. Written down rather than quietly widened: a `players.read` for
// viewers is a 09 §3 change, not a handler's call.
func (h *Instances) readPlayerList(list instance.PlayerList) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := caller(w, r)
		if !ok {
			return
		}
		// Inline, both of them, in each closure: the authorization has to be visible at the
		// route (ADR-037). These four lines used to live in a shared helper, where the
		// call-site guard could not see them and did not check these two routes at all.
		id := r.PathValue("id")
		if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.PlayersManage, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}
		data, err := instance.ReadWorldFile(inst.DataDir, string(list))
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		w.Header().Set("ETag", listETag(data))
		JSON(w, r, http.StatusOK, playerListView{IDs: instance.ParsePlayerList(data)})
	}
}

// writePlayerList is PUT /instances/{id}/{admins,bans,permitted} — a full replacement, and
// one of exactly three in the API (11 §1.1), because the caller holds the whole document.
//
// If-Match is required, not optional. Two co-admins editing adminlist.txt from two
// browsers is 01 §2's primary user, not a hypothetical, and silently keeping the second
// write is data loss with a plausible cover story.
func (h *Instances) writePlayerList(list instance.PlayerList) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := caller(w, r)
		if !ok {
			return
		}
		// Inline, both of them, in each closure: the authorization has to be visible at the
		// route (ADR-037). These four lines used to live in a shared helper, where the
		// call-site guard could not see them and did not check these two routes at all.
		id := r.PathValue("id")
		if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.PlayersManage, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}

		var body playerListView
		if err := Decode(r, &body); err != nil {
			apierr.Write(w, r, err)
			return
		}
		clean, violations := instance.NormalisePlayerIDs(body.IDs)
		if err := playerIDValidation(violations).Err(); err != nil {
			apierr.Write(w, r, err)
			return
		}

		current, err := instance.ReadWorldFile(inst.DataDir, string(list))
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		if !h.matchesCurrent(w, r, current) {
			return
		}

		// The comments the file already had go back into it. The game ships all three
		// files with a header line (measured against build 21981559), so a rewrite that
		// dropped them would erase it on the operator's first save.
		next := instance.FormatPlayerList(instance.PlayerListComments(current), clean)
		if err := instance.WriteWorldFile(inst.DataDir, string(list), next); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
			UserID: u.ID, InstanceID: inst.ID, Action: "instances.players." + string(list) + ".write",
			Detail: fmt.Sprintf("%d entries", len(clean)),
		}); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}

		// The ETag of what was just written, so a client can save twice without re-reading.
		w.Header().Set("ETag", listETag(next))
		JSON(w, r, http.StatusOK, playerListView{IDs: clean})
	}
}

// matchesCurrent enforces 11 §1.1's precondition. A missing If-Match is a client that never
// implemented the check — 400, naming the header — while a mismatch is a real concurrent
// edit, which is what 412 stale_write is for. Distinguishing the two is what lets a client
// tell "I have a bug" from "reload and try again".
func (h *Instances) matchesCurrent(w http.ResponseWriter, r *http.Request, current []byte) bool {
	match := r.Header.Get("If-Match")
	if match == "" {
		apierr.Write(w, r, apierr.New(apierr.InvalidParameter).With("parameter", "If-Match"))
		return false
	}
	if match != listETag(current) {
		apierr.Write(w, r, apierr.New(apierr.StaleWrite))
		return false
	}
	return true
}

// playerListCaller resolves the caller and the instance behind both handlers: D2's 404 for
// an instance this caller cannot see, then D1's own Can() for the action itself.
// playerIDValidation turns instance's rule violations into 11 §2.4's field errors, one per
// bad row, addressed by index so the UI can highlight the line the user typed.
func playerIDValidation(violations []instance.PlayerIDViolation) *apierr.Validation {
	val := &apierr.Validation{}
	for _, v := range violations {
		field := fmt.Sprintf("ids.%d", v.Index)
		switch v.Rule {
		case instance.RuleIDHasWhitespace:
			val.Add(field, apierr.FieldInvalid,
				"An entry cannot contain spaces: the file is one id per line, and the game reads the whole line.")
		case instance.RuleIDNotPrintable:
			val.Add(field, apierr.FieldInvalid, "An entry cannot contain control characters.")
		case instance.RuleIDLooksCommented:
			val.Add(field, apierr.FieldInvalid,
				"This is a comment, not a player id. Comments already in the file are kept automatically.")
		}
	}
	return val
}
