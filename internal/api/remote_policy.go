package api

import (
	"github.com/valminhq/valmin/internal/store"
)

func (b *patchInstanceRequest) remotePolicy() bool {
	return b.RemoteBackupEnabled != nil || b.RemoteKeepCold != nil || b.RemoteKeepHot != nil ||
		b.RemoteKeepSnapshots != nil
}

// remotePolicyUpdate is the remote-backup fields the body sets, merged over current, and what
// they change; nil when the body sets none.
func remotePolicyUpdate(current *store.Instance, body *patchInstanceRequest) (*store.RemotePolicy, []change) {
	if !body.remotePolicy() {
		return nil, nil
	}
	p := store.RemotePolicy{
		Enabled: current.RemoteBackupEnabled, KeepCold: current.RemoteKeepCold,
		KeepHot: current.RemoteKeepHot, KeepSnapshots: current.RemoteKeepSnapshots,
	}
	if body.RemoteBackupEnabled != nil {
		p.Enabled = *body.RemoteBackupEnabled
	}
	if body.RemoteKeepCold != nil {
		p.KeepCold = *body.RemoteKeepCold
	}
	if body.RemoteKeepHot != nil {
		p.KeepHot = *body.RemoteKeepHot
	}
	if body.RemoteKeepSnapshots != nil {
		p.KeepSnapshots = *body.RemoteKeepSnapshots
	}
	var out []change
	out = fieldChange(out, "remote_backup_enabled", current.RemoteBackupEnabled, p.Enabled)
	out = fieldChange(out, "remote_keep_cold", current.RemoteKeepCold, p.KeepCold)
	out = fieldChange(out, "remote_keep_hot", current.RemoteKeepHot, p.KeepHot)
	out = fieldChange(out, "remote_keep_snapshots", current.RemoteKeepSnapshots, p.KeepSnapshots)
	return &p, out
}
