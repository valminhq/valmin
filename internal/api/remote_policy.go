package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/store"
)

func (b *patchInstanceRequest) remotePolicy() bool {
	return b.RemoteBackupEnabled != nil || b.RemoteKeepCold != nil || b.RemoteKeepHot != nil ||
		b.RemoteKeepSnapshots != nil
}

func (h *Instances) applyRemotePolicy(
	w http.ResponseWriter,
	r *http.Request,
	current *store.Instance,
	body *patchInstanceRequest,
) ([]change, bool) {
	if !body.remotePolicy() {
		return nil, true
	}
	enabled, cold, hot, snapshots := current.RemoteBackupEnabled, current.RemoteKeepCold, current.RemoteKeepHot, current.RemoteKeepSnapshots
	if body.RemoteBackupEnabled != nil {
		enabled = *body.RemoteBackupEnabled
	}
	if body.RemoteKeepCold != nil {
		cold = *body.RemoteKeepCold
	}
	if body.RemoteKeepHot != nil {
		hot = *body.RemoteKeepHot
	}
	if body.RemoteKeepSnapshots != nil {
		snapshots = *body.RemoteKeepSnapshots
	}
	if err := h.DB.UpdateRemotePolicy(r.Context(), current.ID, enabled, cold, hot, snapshots); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	var out []change
	out = fieldChange(out, "remote_backup_enabled", current.RemoteBackupEnabled, enabled)
	out = fieldChange(out, "remote_keep_cold", current.RemoteKeepCold, cold)
	out = fieldChange(out, "remote_keep_hot", current.RemoteKeepHot, hot)
	out = fieldChange(out, "remote_keep_snapshots", current.RemoteKeepSnapshots, snapshots)
	return out, true
}
