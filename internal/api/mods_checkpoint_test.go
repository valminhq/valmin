package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/manager"
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
			seed(t, db, `UPDATE instances SET setup_before_update = ? WHERE id = 'inst-a'`, string(mode))
			installOK(t, rt, admin, "Ns-Only", "1.0.0")
			giveWorld(t, dataDir)

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
			if len(setups) != 2 {
				t.Fatalf("automatic setups = %+v, want the install's and the update's", setups)
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

// TestInstallAndRemovalSaveASetupWithoutAWorld asserts an install that replaces nothing and an
// uninstall each save the setup before them as an automatic one named after the mods, recorded
// as the requester's and linked to no world backup, while archiving no world.
func TestInstallAndRemovalSaveASetupWithoutAWorld(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	giveWorld(t, dataDir)

	installOK(t, rt, admin, "Ns-Only", "1.0.0")
	rec := deleteMod(t, rt, admin, "Ns-Only", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("uninstall = %d %s, want 202", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstall job = %+v, want succeeded", got)
	}

	if backups := preUpdateBackupIDs(t, db); len(backups) != 0 {
		t.Errorf("pre_update backups = %v, want none", backups)
	}
	setups := automaticSetups(t, db)
	names := make([]string, 0, len(setups))
	for _, s := range setups {
		names = append(names, s.Name)
		if s.BackupID != "" || s.CreatedByName != admin.Username {
			t.Errorf("setup %q linked to %q by %q, want no world and %q", s.Name, s.BackupID,
				s.CreatedByName, admin.Username)
		}
	}
	if want := []string{"Before removing Ns-Only", "Before installing Ns-Only"}; !slices.Equal(names, want) {
		t.Errorf("automatic setups = %q, want %q", names, want)
	}
}

// TestModUpdatesKeepTheConfiguredAutomaticSetups asserts a server keeps as many automatic setups
// as it is set to, and the world backups of only the newest linked ones; a released backup goes
// back to retention, and lowering the counts applies at once.
func TestModUpdatesKeepTheConfiguredAutomaticSetups(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Only", "1.0.0")
	giveWorld(t, dataDir)
	seed(t, db, `UPDATE instances SET setup_versions_kept = 4, setup_worlds_kept = 2 WHERE id = 'inst-a'`)

	for _, v := range []string{"2.0.0", "1.0.0", "2.0.0", "1.0.0"} {
		installOK(t, rt, admin, "Ns-Only", v)
	}

	if setups := automaticSetups(t, db); len(setups) != 4 {
		t.Fatalf("automatic setups = %d, want 4", len(setups))
	}
	backups := preUpdateBackupIDs(t, db)
	if len(backups) != 4 {
		t.Fatalf("pre_update backups = %v, want four", backups)
	}
	pinned := func() []bool {
		out := make([]bool, 0, len(backups))
		for _, id := range backups {
			p, err := db.BackupPinned(t.Context(), "inst-a", id)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	if got, want := pinned(), []bool{false, false, true, true}; !slices.Equal(got, want) {
		t.Errorf("backups pinned = %v, want %v", got, want)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]int{"setup_versions_kept": 1, "setup_worlds_kept": 0})))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s, want 200", rec.Code, rec.Body)
	}
	if setups := automaticSetups(t, db); len(setups) != 1 {
		t.Errorf("automatic setups after lowering = %d, want 1", len(setups))
	}
	if got, want := pinned(), []bool{false, false, false, false}; !slices.Equal(got, want) {
		t.Errorf("backups pinned after lowering = %v, want %v", got, want)
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

// TestInstallsThatCreateAServerSaveNoSetup asserts the minimum installs a definition chain
// submits while it creates a server save no automatic setup.
func TestInstallsThatCreateAServerSaveNoSetup(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, _ := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	job, err := rt.mods.install.SubmitInstall(t.Context(), inst,
		manager.PackageRequest{FullName: "Ns-Only", Version: "1.0.0"}, admin.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitJob(t, rt, admin, job.ID); got.Status != "succeeded" {
		t.Fatalf("install job = %+v, want succeeded", got)
	}
	if setups := automaticSetups(t, db); len(setups) != 0 {
		t.Errorf("automatic setups = %+v, want none", setups)
	}
}

// savePending saves a setup of inst-a that waits for the next mod change's world backup and
// returns its ID.
func savePending(t *testing.T, rt *Server, db *store.DB, admin *store.User, name string) string {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, setupsURL,
		jsonBody(t, map[string]any{"name": name, "link_next_world_backup": true})))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("save = %d %s, want 202", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "succeeded" {
		t.Fatalf("save job = %+v, want succeeded", job)
	}
	rows, err := db.ListSetups(t.Context(), "inst-a")
	if err != nil || len(rows) == 0 || !rows[0].WorldPending {
		t.Fatalf("newest setup = %+v, %v, want one waiting for a world backup", rows, err)
	}
	return rows[0].ID
}

// setupRow reads one setup of inst-a.
func setupRow(t *testing.T, db *store.DB, id string) *store.SavedSetup {
	t.Helper()
	row, _, err := db.SetupByID(t.Context(), "inst-a", id)
	if err != nil || row == nil {
		t.Fatalf("setup %s = %+v, %v", id, row, err)
	}
	return row
}

// TestAWaitingSetupGetsTheNextUpdatesWorldBackup asserts a setup saved to wait for a world backup
// is linked to the one the next update takes while the mods are still its own, and stops waiting
// without one once they change first, even back to the same set, or change without a world to
// back up.
func TestAWaitingSetupGetsTheNextUpdatesWorldBackup(t *testing.T) {
	t.Parallel()
	t.Run("mods unchanged", func(t *testing.T) {
		t.Parallel()
		rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
		alreadyModded(t, db)
		installOK(t, rt, admin, "Ns-Only", "1.0.0")
		giveWorld(t, dataDir)
		id := savePending(t, rt, db, admin, "known good")

		installOK(t, rt, admin, "Ns-Only", "2.0.0")

		backups := preUpdateBackupIDs(t, db)
		if row := setupRow(t, db, id); len(backups) != 1 || row.BackupID != backups[0] || row.WorldPending {
			t.Errorf("setup linked to %q, waiting %v, want %v and done", row.BackupID, row.WorldPending, backups)
		}
	})
	t.Run("mods changed in between", func(t *testing.T) {
		t.Parallel()
		rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
		alreadyModded(t, db)
		installOK(t, rt, admin, "Ns-Only", "2.0.0")
		giveWorld(t, dataDir)
		id := savePending(t, rt, db, admin, "on 2.0.0")
		rec := deleteMod(t, rt, admin, "Ns-Only", "")
		var accepted jobView
		decodeInto(t, rec, &accepted)
		if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "succeeded" {
			t.Fatalf("uninstall job = %+v, want succeeded", job)
		}
		installOK(t, rt, admin, "Ns-Only", "1.0.0")

		installOK(t, rt, admin, "Ns-Only", "2.0.0")

		if row := setupRow(t, db, id); row.BackupID != "" || row.WorldPending {
			t.Errorf("setup linked to %q, waiting %v, want neither", row.BackupID, row.WorldPending)
		}
	})
	t.Run("mods changed and changed back", func(t *testing.T) {
		t.Parallel()
		rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
		alreadyModded(t, db)
		installOK(t, rt, admin, "Ns-Only", "1.0.0")
		giveWorld(t, dataDir)
		id := savePending(t, rt, db, admin, "before a toggle")
		toggleMod(t, rt, admin, "Ns-Only", false)
		toggleMod(t, rt, admin, "Ns-Only", true)

		installOK(t, rt, admin, "Ns-Only", "2.0.0")

		if row := setupRow(t, db, id); row.BackupID != "" || row.WorldPending {
			t.Errorf("setup linked to %q, waiting %v, want neither", row.BackupID, row.WorldPending)
		}
	})
	t.Run("no world", func(t *testing.T) {
		t.Parallel()
		rt, db, admin, _, _ := installWorld(t, twoVersions()...)
		alreadyModded(t, db)
		installOK(t, rt, admin, "Ns-Only", "1.0.0")
		id := savePending(t, rt, db, admin, "no world")

		installOK(t, rt, admin, "Ns-Only", "2.0.0")

		if row := setupRow(t, db, id); row.BackupID != "" || row.WorldPending {
			t.Errorf("setup linked to %q, waiting %v, want neither", row.BackupID, row.WorldPending)
		}
	})
}
