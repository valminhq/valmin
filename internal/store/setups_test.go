package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
)

func TestSavedSetupBackupLinkAndArtifactReferences(t *testing.T) {
	db := open(t)
	seedUser(t, db, "admin")
	seedInstance(t, db, "server-a", 28000)
	seedInstance(t, db, "server-b", 28010)

	for _, b := range []Backup{
		{ID: "good", InstanceID: "server-a", Path: "/tmp/good", SHA256: "hash", WorldName: "Worldserver-a", Trigger: TriggerManual, Consistent: true},
		{ID: "hot", InstanceID: "server-a", Path: "/tmp/hot", SHA256: "hash", WorldName: "Worldserver-a", Trigger: TriggerManual},
		{ID: "foreign", InstanceID: "server-b", Path: "/tmp/foreign", SHA256: "hash", WorldName: "Worldserver-b", Trigger: TriggerManual, Consistent: true},
	} {
		if err := db.CreateBackup(t.Context(), &b); err != nil {
			t.Fatalf("CreateBackup(%s): %v", b.ID, err)
		}
	}

	for _, id := range []string{"hot", "foreign", "missing"} {
		s := &SavedSetup{
			ID:           id,
			InstanceID:   "server-a",
			Name:         "Working",
			CreatedBy:    "admin",
			SnapshotJSON: "{}",
			BackupID:     id,
		}
		if err := db.SaveSetup(t.Context(), s, nil); !errors.Is(err, ErrInvalidSetupBackup) {
			t.Errorf("SaveSetup(%s) = %v, want ErrInvalidSetupBackup", id, err)
		}
	}
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := SetupArtifactRef{FullName: "same-name", Source: "hexium", Version: "1.0.0", Kind: "files", SHA256: digest}
	for _, id := range []string{"first", "second"} {
		s := &SavedSetup{
			ID: id, InstanceID: "server-a", Name: "Working", CreatedBy: "admin",
			GameBuildID: "build", WorldName: "Worldserver-a", SnapshotJSON: "{}", BackupID: "good",
		}
		if err := db.SaveSetup(t.Context(), s, []SetupArtifactRef{ref}); err != nil {
			t.Fatalf("SaveSetup(%s): %v", id, err)
		}
	}
	list, err := db.ListSetups(t.Context(), "server-a")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListSetups = %+v, %v, want two", list, err)
	}
	one, refs, err := db.SetupByID(t.Context(), "server-a", "first")
	if err != nil || one == nil || one.BackupID != "good" || len(refs) != 1 || refs[0] != ref {
		t.Fatalf("SetupByID = %+v, %+v, %v", one, refs, err)
	}
	foreign, _, err := db.SetupByID(t.Context(), "server-b", "first")
	if err != nil || foreign != nil {
		t.Fatalf("foreign SetupByID = %+v, %v", foreign, err)
	}
	pinned, err := db.BackupPinned(t.Context(), "server-a", "good")
	if err != nil || !pinned {
		t.Fatalf("BackupPinned = %v, %v", pinned, err)
	}
	if err := db.DeleteBackup(t.Context(), "server-a", "good"); err == nil {
		t.Fatal("DeleteBackup removed a linked backup")
	}
	if err := db.DeleteSetup(t.Context(), "server-a", "first"); err != nil {
		t.Fatalf("DeleteSetup(first): %v", err)
	}
	referenced, err := db.ReferencedSetupArtifacts(t.Context())
	if err != nil || !referenced[digest] {
		t.Fatalf("ReferencedSetupArtifacts = %+v, %v", referenced, err)
	}
	pinned, err = db.BackupPinned(t.Context(), "server-a", "good")
	if err != nil || !pinned {
		t.Fatalf("remaining setup lost backup link: %v, %v", pinned, err)
	}
	if err := db.DeleteSetup(t.Context(), "server-a", "second"); err != nil {
		t.Fatalf("DeleteSetup(second): %v", err)
	}
	referenced, err = db.ReferencedSetupArtifacts(t.Context())
	if err != nil || referenced[digest] {
		t.Fatalf("orphan digest still referenced: %+v, %v", referenced, err)
	}
	pinned, err = db.BackupPinned(t.Context(), "server-a", "good")
	if err != nil || pinned {
		t.Fatalf("BackupPinned after delete = %v, %v", pinned, err)
	}
	if err := db.DeleteBackup(t.Context(), "server-a", "good"); err != nil {
		t.Fatalf("DeleteBackup after unlink: %v", err)
	}
	if err := db.DeleteSetup(t.Context(), "server-a", "second"); !errors.Is(err, ErrSetupNotFound) {
		t.Errorf("second DeleteSetup = %v, want ErrSetupNotFound", err)
	}
}

func TestSavedSetupRestoreStateIsExact(t *testing.T) {
	db := open(t)
	seedInstance(t, db, "server", 28000)
	old := InstanceMod{
		InstanceID: "server", FullName: "old", Source: source.Thunderstore,
		Version: "1.0.0", InstalledAs: InstalledExplicit, Side: SideUnknown,
		Enabled: true, FileManifest: "[]",
	}
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := TxUpsertInstanceMods(t.Context(), tx, []InstanceMod{old}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mods := []InstanceMod{
		{
			InstanceID: "server", FullName: "denikson-BepInExPack_Valheim", Source: source.Hexium,
			Version: "5.4.1", InstalledAs: InstalledExplicit, Side: "server_only",
			Enabled: true, Locked: true, FileManifest: "[]",
		},
		{
			InstanceID: "server", FullName: "disabled", Source: source.Thunderstore,
			Version: "2.0.0", InstalledAs: InstalledDependency, Side: "client_optional",
			Enabled: false, Locked: true, FileManifest: "[]",
		},
	}
	launch := InstanceLaunch{
		ServerName: "Restored", Password: "ignore-this",
		Public: true, Crossplay: true, MemLimitMB: 8192,
	}
	backup := BackupPolicy{KeepCold: 7, KeepHot: 8, OnRestart: true}
	tx, err = db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := TxApplySetupState(t.Context(), tx, "server", mods, launch, backup); err != nil {
		t.Fatalf("TxApplySetupState: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	inst, err := db.InstanceByID(t.Context(), "server")
	if err != nil || inst == nil {
		t.Fatalf("InstanceByID = %+v, %v", inst, err)
	}
	if inst.WorldName != "Worldserver" || inst.ServerName != "Restored" ||
		!inst.Modded || inst.BepInExVersion == nil || *inst.BepInExVersion != "5.4.1" ||
		!inst.RestartRequired || inst.BackupKeepCold != 7 || inst.BackupKeepHot != 8 ||
		!inst.BackupOnRestart {
		t.Errorf("restored instance = %+v", inst)
	}
	var password string
	if err := db.Reader.QueryRowContext(t.Context(), "SELECT password FROM instances WHERE id = ?", "server").
		Scan(&password); err != nil {
		t.Fatal(err)
	}
	if password != "v1.k.n.ct" {
		t.Errorf("password = %q, want original encrypted value", password)
	}
	got, err := db.InstanceMods(t.Context(), "server")
	if err != nil || len(got) != 2 {
		t.Fatalf("InstanceMods = %+v, %v", got, err)
	}
	if got[1].FullName != "disabled" || got[1].Enabled || !got[1].Locked ||
		got[1].Side != "client_optional" || got[1].Source != source.Thunderstore {
		t.Errorf("disabled mod state = %+v", got[1])
	}
	if strings.Contains(got[0].FullName, "old") || strings.Contains(got[1].FullName, "old") {
		t.Errorf("old mod survived restore: %+v", got)
	}

	exec(t, db.Writer, "UPDATE instances SET state = 'running' WHERE id = ?", "server")
	tx, err = db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = TxApplySetupState(t.Context(), tx, "server", nil, launch, backup)
	_ = tx.Rollback()
	if !errors.Is(err, ErrInstanceNotStopped) {
		t.Errorf("running restore = %v, want ErrInstanceNotStopped", err)
	}
}
