package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// updateStatusView compares the installed build with the head of the instance's branch.
// BranchBuildID is null for a branch Steam does not list, a password branch, and so is
// UpdateAvailable.
type updateStatusView struct {
	InstalledBuildID *string    `json:"installed_build_id"`
	PublicBuildID    *string    `json:"public_build_id"`
	Branch           string     `json:"branch"`
	BranchBuildID    *string    `json:"branch_build_id"`
	ObservedAt       *time.Time `json:"observed_at"`
	UpdateAvailable  *bool      `json:"update_available"`
}

// updateStatus reports the last successful observation; failed checks never replace it.
func (h *Instances) updateStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	v, err := observeUpdateStatus(r.Context(), h.DB, inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, v)
}

// observeUpdateStatus is inst's installed build against the last successful observation of its
// branch.
func observeUpdateStatus(ctx context.Context, db *store.DB, inst *store.Instance) (updateStatusView, error) {
	var observed instance.PublicBuild
	found, err := db.KVGet(ctx, instance.PublicBuildKey, &observed)
	if err != nil {
		return updateStatusView{}, fmt.Errorf("read the observed public build: %w", err)
	}
	v := updateStatusView{Branch: inst.GameBranch}
	// The manifest under server/ is what the instance actually runs; the column is a cache of
	// it, and a game update whose recovery completed the swap without reaching its Finish
	// transaction leaves the two disagreeing. Legacy rows carry a cache alias rather than a
	// version, which the column could not answer either way.
	// A row with no build never finished provisioning, so whatever server/ holds is a partial
	// clone, not an install.
	var installed string
	sameBranch := false
	if inst.GameBuildID != nil {
		installed, err = instance.InstalledBuildID(inst.DataDir)
		if err != nil {
			installed = *inst.GameBuildID
		} else if branch, err := instance.ServerBranch(instance.ServerDir(inst.DataDir)); err == nil {
			sameBranch = branch == inst.GameBranch
		}
	}
	if knownBuildID(installed) {
		v.InstalledBuildID = &installed
	}
	if found && knownBuildID(observed.BuildID) && !observed.ObservedAt.IsZero() {
		v.PublicBuildID, v.ObservedAt = &observed.BuildID, &observed.ObservedAt
		if head := observed.BranchBuilds()[inst.GameBranch]; knownBuildID(head) {
			v.BranchBuildID = &head
			if v.InstalledBuildID != nil {
				available := updateAvailable(installed, head, sameBranch)
				v.UpdateAvailable = &available
			}
		}
	}
	return v, nil
}

// updateAvailable reports whether head would replace installed. On the instance's own branch
// only a newer head counts; from another or an unknown branch, any difference does.
func updateAvailable(installed, head string, sameBranch bool) bool {
	if !sameBranch {
		return installed != head
	}
	i, _ := strconv.ParseUint(installed, 10, 64)
	h, _ := strconv.ParseUint(head, 10, 64)
	return i < h
}

func knownBuildID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0
}

// newBuildNotification owes an update-available notification when the build just observed is
// not the one already recorded. Nil when there is nothing to say, or no notifier wired.
