package store

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/mods/source"
)

func TestInstanceModVersionMissingIsFalseNotError(t *testing.T) {
	t.Parallel()
	db := open(t)
	_, _, ok, err := db.InstanceModVersion(t.Context(), "inst-a", "Nobody-Home")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true for an uninstalled package, want false")
	}
}

func TestInstanceModVersionReadsTheInstalledRow(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()

	exec(t, db.Writer, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, ?, ?, 'v1.k.n.ct', ?, ?, ?)`,
		"inst-a", "inst-a", "/srv/valmin/instances/inst-a", 2456,
		"Server", "World", "cp-a", Now(), Now())
	exec(t, db.Writer, `INSERT INTO instance_mods (
		instance_id, full_name, source, version, installed_as, file_manifest, installed_at
	) VALUES ('inst-a', 'ValheimModding-Jotunn', 'hexium', '2.29.2', 'explicit', '[]', ?)`, Now())

	version, src, ok, err := db.InstanceModVersion(ctx, "inst-a", "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || version != "2.29.2" {
		t.Errorf("version = %q, ok = %v, want 2.29.2, true", version, ok)
	}
	// The registry is read back with the version: an installed package is never re-sourced.
	if src != source.Hexium {
		t.Errorf("source = %v, want hexium", src)
	}
}

// TestInstanceModsCataloguedJoinsTheInstalledRegistryOnly is Q39's link: one query answers the
// catalogue question for every installed package, and only from the registry the files came
// from, since another registry's latest version describes different bytes (B14).
func TestInstanceModsCataloguedJoinsTheInstalledRegistryOnly(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()

	exec(t, db.Writer, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', 'stopped', '/srv/valmin/instances/inst-a', 2456,
		'Server', 'World', 'v1.k.n.ct', 'cp-a', ?, ?)`, Now(), Now())
	for _, row := range [][2]string{
		{"ValheimModding-Jotunn", "thunderstore"},
		{"Only-Elsewhere", "thunderstore"},
		{"Never-Synced", "thunderstore"},
	} {
		exec(t, db.Writer, `INSERT INTO instance_mods (
			instance_id, full_name, source, version, installed_as, file_manifest, installed_at
		) VALUES ('inst-a', ?, ?, '1.0.0', 'explicit', '[]', ?)`, row[0], row[1], Now())
	}
	if err := db.UpsertModPackages(ctx, []ModPackage{
		{
			FullName: "ValheimModding-Jotunn", Source: source.Thunderstore, Namespace: "ValheimModding",
			Name: "Jotunn", LatestVersion: "2.0.0", IsDeprecated: true, CategoriesJSON: "[]",
		},
		{
			FullName: "Only-Elsewhere", Source: source.Hexium, Namespace: "Only", Name: "Elsewhere",
			LatestVersion: "9.0.0", CategoriesJSON: "[]",
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	got, err := db.InstanceModsCatalogued(ctx, "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want every installed package once", len(got))
	}
	byName := map[string]CataloguedMod{}
	for _, c := range got {
		byName[c.FullName] = c
	}
	jotunn := byName["ValheimModding-Jotunn"].Package
	if jotunn == nil || jotunn.LatestVersion != "2.0.0" || !jotunn.IsDeprecated || jotunn.Name != "Jotunn" {
		t.Errorf("Jotunn's catalogue row = %+v", jotunn)
	}
	if p := byName["Only-Elsewhere"].Package; p != nil {
		t.Errorf("a package installed from one registry joined the other's row: %+v", p)
	}
	if p := byName["Never-Synced"].Package; p != nil {
		t.Errorf("a package no catalogue has joined a row: %+v", p)
	}
}

// TestInstanceModsCataloguedCarriesTheUnlistedStamp asserts the join reports when a complete
// listing omitted a catalogue row, the one fact that tells a package pulled upstream from one
// still listed, since a sync never deletes a row. Listing the package again clears the stamp.
func TestInstanceModsCataloguedCarriesTheUnlistedStamp(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()

	exec(t, db.Writer, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', 'stopped', '/srv/valmin/instances/inst-a', 2456,
		'Server', 'World', 'v1.k.n.ct', 'cp-a', ?, ?)`, Now(), Now())
	for _, name := range []string{"Still-Listed", "Pulled", "Never-Listed"} {
		exec(t, db.Writer, `INSERT INTO instance_mods (
			instance_id, full_name, source, version, installed_as, file_manifest, installed_at
		) VALUES ('inst-a', ?, 'thunderstore', '1.0.0', 'explicit', '[]', ?)`, name, Now())
	}
	upsert := func(names ...string) {
		t.Helper()
		rows := make([]ModPackage, 0, len(names))
		for _, name := range names {
			rows = append(rows, ModPackage{
				FullName: name, Source: source.Thunderstore, Namespace: "Ns", Name: name,
				LatestVersion: "1.0.0", CategoriesJSON: "[]",
			})
		}
		if err := db.UpsertModPackages(ctx, rows, nil); err != nil {
			t.Fatal(err)
		}
	}
	unlisted := func() map[string]*CataloguedMod {
		t.Helper()
		got, err := db.InstanceModsCatalogued(ctx, "inst-a")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*CataloguedMod{}
		for i := range got {
			out[got[i].FullName] = &got[i]
		}
		return out
	}

	upsert("Still-Listed", "Pulled")
	if err := db.MarkUnlisted(ctx, source.Thunderstore, map[string]bool{"Still-Listed": true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := unlisted()
	if c := got["Still-Listed"]; c.Package == nil || c.UnlistedAt != "" {
		t.Errorf("Still-Listed = %+v, %q, want a row and no stamp", c.Package, c.UnlistedAt)
	}
	if c := got["Pulled"]; c.Package == nil || c.UnlistedAt == "" {
		t.Errorf("Pulled = %+v, %q, want a row with an unlisted stamp", c.Package, c.UnlistedAt)
	}
	if c := got["Never-Listed"]; c.Package != nil || c.UnlistedAt != "" {
		t.Errorf("Never-Listed = %+v, %q, want no row and no stamp", c.Package, c.UnlistedAt)
	}

	upsert("Pulled")
	if c := unlisted()["Pulled"]; c.UnlistedAt != "" {
		t.Errorf("Pulled after being listed again: UnlistedAt = %q, want none", c.UnlistedAt)
	}
}
