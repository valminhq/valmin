package store

import (
	"reflect"
	"testing"
)

func TestModArchiveUseCountsServersAndSetups(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedUser(t, db, "admin")
	seedInstance(t, db, "server-a", 28000)
	seedInstance(t, db, "server-b", 28010)
	for _, row := range [][3]string{
		{"server-a", "Shared-Mod", "1.0.0"},
		{"server-b", "Shared-Mod", "1.0.0"},
		{"server-b", "Only-B", "2.0.0"},
	} {
		exec(t, db.Writer, `INSERT INTO instance_mods (
			instance_id, full_name, source, version, installed_as, file_manifest, installed_at
		) VALUES (?, ?, 'thunderstore', ?, 'explicit', '[]', ?)`, row[0], row[1], row[2], Now())
	}
	if err := db.CreateBackup(t.Context(), &Backup{
		ID: "good", InstanceID: "server-a", Path: "/tmp/good", SHA256: "hash",
		WorldName: "Worldserver-a", Trigger: TriggerManual, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	snapshots := map[string]string{
		"first": `{"mods":[{"full_name":"Shared-Mod","source":"thunderstore","version":"0.9.0"},` +
			`{"full_name":"Hex-Mod","source":"hexium","version":"3.0.0"}]}`,
		"second": `{"mods":[{"full_name":"Shared-Mod","source":"thunderstore","version":"0.9.0"},` +
			`{"full_name":"Old-Mod","version":"1.2.0"}]}`,
	}
	const digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, id := range []string{"first", "second"} {
		s := &SavedSetup{
			ID: id, InstanceID: "server-a", Name: id, CreatedBy: "admin", GameBuildID: "build",
			WorldName: "Worldserver-a", SnapshotJSON: snapshots[id], BackupID: "good",
		}
		ref := SetupArtifactRef{FullName: "Hex-Mod", Source: "hexium", Version: "3.0.0", Kind: "zip", SHA256: digest}
		if err := db.SaveSetup(t.Context(), s, []SetupArtifactRef{ref}); err != nil {
			t.Fatalf("SaveSetup(%s): %v", id, err)
		}
	}

	use, err := db.ModArchiveUse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[ModArchive]ArchiveUse{
		{"thunderstore", "Shared-Mod", "1.0.0"}: {InstanceIDs: []string{"server-a", "server-b"}},
		{"thunderstore", "Only-B", "2.0.0"}:     {InstanceIDs: []string{"server-b"}},
		{"thunderstore", "Shared-Mod", "0.9.0"}: {Setups: 2},
		{"hexium", "Hex-Mod", "3.0.0"}:          {Setups: 1},
		{"thunderstore", "Old-Mod", "1.2.0"}:    {Setups: 1},
	}
	got := map[ModArchive]ArchiveUse{}
	for a, u := range use {
		got[a] = *u
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ModArchiveUse = %+v, want %+v", got, want)
	}

	blobs, err := db.SetupArtifactUse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blobs, map[string]int{digest: 2}) {
		t.Errorf("SetupArtifactUse = %v, want %s held by 2 setups", blobs, digest)
	}
}
