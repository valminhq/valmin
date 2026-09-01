package store

import "testing"

func TestInstanceModVersionMissingIsFalseNotError(t *testing.T) {
	db := open(t)
	_, ok, err := db.InstanceModVersion(t.Context(), "inst-a", "Nobody-Home")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true for an uninstalled package, want false")
	}
}

func TestInstanceModVersionReadsTheInstalledRow(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	exec(t, db.Writer, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, ?, ?, 'v1.k.n.ct', ?, ?, ?)`,
		"inst-a", "inst-a", "/srv/valmin/instances/inst-a", 2456,
		"Server", "World", "cp-a", Now(), Now())
	exec(t, db.Writer, `INSERT INTO instance_mods (
		instance_id, full_name, version, installed_as, file_manifest, installed_at
	) VALUES ('inst-a', 'ValheimModding-Jotunn', '2.29.2', 'explicit', '[]', ?)`, Now())

	version, ok, err := db.InstanceModVersion(ctx, "inst-a", "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || version != "2.29.2" {
		t.Errorf("version = %q, ok = %v, want 2.29.2, true", version, ok)
	}
}
