package store

import "testing"

func TestUpsertModPackagesWritesBothTables(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	err := db.UpsertModPackages(ctx,
		[]ModPackage{{
			FullName: "ValheimModding-Jotunn", Namespace: "ValheimModding", Name: "Jotunn",
			Description: "A modding library", LatestVersion: "2.29.2",
			Downloads: 1000, Rating: 50, CategoriesJSON: `["Libraries"]`,
		}},
		[]ModVersion{{
			FullName: "ValheimModding-Jotunn", Version: "2.29.2",
			DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2333"]`,
			DownloadURL:      "https://thunderstore.io/package/download/ValheimModding/Jotunn/2.29.2/",
			FileSize:         814792,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.ModPackageByFullName(ctx, "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "ValheimModding" || got.LatestVersion != "2.29.2" || got.Downloads != 1000 {
		t.Errorf("ModPackageByFullName = %+v", got)
	}

	versions, err := db.ModVersionsByFullName(ctx, "ValheimModding-Jotunn")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].DependenciesJSON != `["denikson-BepInExPack_Valheim-5.4.2333"]` {
		t.Errorf("ModVersionsByFullName = %+v", versions)
	}
}

// TestUpsertModPackagesReplacesOnConflict is what makes a re-sync idempotent: a package
// whose rating or description changed between syncs must land as the new value, not a
// second row.
func TestUpsertModPackagesReplacesOnConflict(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	seed := func(rating int) {
		if err := db.UpsertModPackages(ctx,
			[]ModPackage{{FullName: "A-B", Namespace: "A", Name: "B", Rating: rating}}, nil,
		); err != nil {
			t.Fatal(err)
		}
	}
	seed(10)
	seed(20)

	var count int
	if err := db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM mod_packages WHERE full_name = 'A-B'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (upsert must replace, not duplicate)", count)
	}

	got, err := db.ModPackageByFullName(ctx, "A-B")
	if err != nil {
		t.Fatal(err)
	}
	if got.Rating != 20 {
		t.Errorf("Rating = %d, want 20 (the second sync's value)", got.Rating)
	}
}

func TestUpsertModPackagesEmptyBatchIsANoOp(t *testing.T) {
	db := open(t)
	if err := db.UpsertModPackages(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
}
