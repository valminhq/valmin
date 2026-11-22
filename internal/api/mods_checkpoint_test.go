package api

import (
	"testing"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// automaticSetups lists inst-a's automatic setups, newest first.
func automaticSetups(t *testing.T, db *store.DB) []store.SavedSetup {
	t.Helper()
	all, err := db.ListSetups(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	var out []store.SavedSetup
	for i := range all {
		if all[i].Automatic {
			out = append(out, all[i])
		}
	}
	return out
}

// preUpdateBackupIDs lists inst-a's pre_update backups, oldest first.
func preUpdateBackupIDs(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Reader.QueryContext(t.Context(), `SELECT id FROM backups
		WHERE instance_id = 'inst-a' AND trigger = ? ORDER BY created_at, id`, store.TriggerPreUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestModUpdateSavesALinkedSetup asserts a mod update saves the setup the server had as an
// automatic one linked to the update's world backup, light or full as the server asks, and
// saves none when the server turns it off while still backing up the world.
func TestModUpdateSavesALinkedSetup(t *testing.T) {
	t.Parallel()
	for _, mode := range []control.SetupBeforeUpdate{
		control.SetupBeforeUpdateFull, control.SetupBeforeUpdateLight, control.SetupBeforeUpdateOff,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
			alreadyModded(t, db)
			installOK(t, rt, admin, "Ns-Only", "1.0.0")
			giveWorld(t, dataDir)
			seed(t, db, `UPDATE instances SET setup_before_update = ? WHERE id = 'inst-a'`, string(mode))

			installOK(t, rt, admin, "Ns-Only", "2.0.0")

			backups := preUpdateBackupIDs(t, db)
			if len(backups) != 1 {
				t.Fatalf("pre_update backups = %v, want one", backups)
			}
			setups := automaticSetups(t, db)
			if mode == control.SetupBeforeUpdateOff {
				if len(setups) != 0 {
					t.Errorf("automatic setups = %+v, want none with the setting off", setups)
				}
				return
			}
			if len(setups) != 1 {
				t.Fatalf("automatic setups = %+v, want one", setups)
			}
			got := setups[0]
			if got.Name != "Before updating Ns-Only" || got.BackupID != backups[0] {
				t.Errorf("setup = %q linked to %q, want \"Before updating Ns-Only\" linked to %q",
					got.Name, got.BackupID, backups[0])
			}
			row, refs, err := db.SetupByID(t.Context(), "inst-a", got.ID)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := control.DecodeSetupSnapshot(row)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Light != (mode == control.SetupBeforeUpdateLight) {
				t.Errorf("light = %v for %s", snap.Light, mode)
			}
			if len(snap.Mods) != 1 || snap.Mods[0].Version != "1.0.0" {
				t.Errorf("saved mods = %+v, want Ns-Only 1.0.0, the version before the update", snap.Mods)
			}
			if mode == control.SetupBeforeUpdateFull && len(refs) != 1 {
				t.Errorf("a full setup keeps %d package files, want 1", len(refs))
			}
		})
	}
}

// TestFreshInstallSavesNoSetup asserts an install that replaces nothing takes no checkpoint.
func TestFreshInstallSavesNoSetup(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	giveWorld(t, dataDir)

	installOK(t, rt, admin, "Ns-Only", "1.0.0")

	if setups := automaticSetups(t, db); len(setups) != 0 {
		t.Errorf("automatic setups = %+v, want none", setups)
	}
}

// TestModUpdatesKeepThreeAutomaticSetups asserts each server keeps its newest three automatic
// setups, and a setup pruned away releases its world backup back to retention.
func TestModUpdatesKeepThreeAutomaticSetups(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Only", "1.0.0")
	giveWorld(t, dataDir)

	for _, v := range []string{"2.0.0", "1.0.0", "2.0.0", "1.0.0"} {
		installOK(t, rt, admin, "Ns-Only", v)
	}

	if setups := automaticSetups(t, db); len(setups) != 3 {
		t.Fatalf("automatic setups = %d, want 3", len(setups))
	}
	backups := preUpdateBackupIDs(t, db)
	if len(backups) != 4 {
		t.Fatalf("pre_update backups = %v, want four", backups)
	}
	for i, id := range backups {
		pinned, err := db.BackupPinned(t.Context(), "inst-a", id)
		if err != nil {
			t.Fatal(err)
		}
		if want := i > 0; pinned != want {
			t.Errorf("backup %d pinned = %v, want %v", i, pinned, want)
		}
	}
}

// TestUpdatePreviewsSaySetupIsSaved asserts the update preview reports the automatic setup the
// apply saves, and stops reporting it once the server turns it off.
func TestUpdatePreviewsSaySetupIsSaved(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := updateWorld(t)

	if !previewUpdatesOf(t, rt, admin).Setup {
		t.Error("setup = false with the default setting")
	}
	seed(t, db, `UPDATE instances SET setup_before_update = 'off' WHERE id = 'inst-a'`)
	if previewUpdatesOf(t, rt, admin).Setup {
		t.Error("setup = true with the setting off")
	}
}
