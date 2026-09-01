package store

import (
	"reflect"
	"testing"
)

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

// TestModPackageByFullNameMissingIsNilNil is the JobByID convention: a caller tells
// "does not exist" from a genuine read failure without inspecting the error's type.
func TestModPackageByFullNameMissingIsNilNil(t *testing.T) {
	db := open(t)
	got, err := db.ModPackageByFullName(t.Context(), "Nobody-Home")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func seedSearchCorpus(t *testing.T, db *DB) {
	t.Helper()
	err := db.UpsertModPackages(t.Context(), []ModPackage{
		{
			FullName: "ValheimModding-Jotunn", Namespace: "ValheimModding", Name: "Jotunn",
			Description: "A modding library", Downloads: 3000, CategoriesJSON: `["Libraries"]`,
		},
		{
			FullName: "Advize-PlantEverything", Namespace: "Advize", Name: "PlantEverything",
			Description: "Plant berry bushes and more", Downloads: 500, CategoriesJSON: `["Mods"]`,
		},
		{
			FullName: "Smoothbrain-Sailing", Namespace: "Smoothbrain", Name: "Sailing",
			Description: "A sailing skill", Downloads: 200, CategoriesJSON: `["Mods","QoL"]`,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSearchModPackagesByNameSubstring(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	got, err := db.SearchModPackages(t.Context(), "jotu", "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "ValheimModding-Jotunn" {
		t.Errorf("got = %+v, want just Jotunn", got)
	}
}

func TestSearchModPackagesByDescriptionSubstring(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	got, err := db.SearchModPackages(t.Context(), "berry", "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "Advize-PlantEverything" {
		t.Errorf("got = %+v, want just PlantEverything", got)
	}
}

func TestSearchModPackagesByCategory(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	got, err := db.SearchModPackages(t.Context(), "", "QoL", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "Smoothbrain-Sailing" {
		t.Errorf("got = %+v, want just Sailing", got)
	}
}

// TestSearchModPackagesEscapesLikeWildcards proves a literal "%" in a search term is not
// treated as a wildcard — a search for "100%" must not match every row.
func TestSearchModPackagesEscapesLikeWildcards(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	got, err := db.SearchModPackages(t.Context(), "100%", "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows for a literal-%%-only term, want 0", len(got))
	}
}

// TestSearchModPackagesPaginatesWithoutDuplicateOrSkip is ADR-035's whole reason for
// existing: paging through with limit=1 must see every row exactly once, alphabetically.
func TestSearchModPackagesPaginatesWithoutDuplicateOrSkip(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	var seen []string
	afterName, afterFullName := "", ""
	for {
		page, err := db.SearchModPackages(t.Context(), "", "", afterName, afterFullName, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].FullName)
		afterName, afterFullName = page[0].Name, page[0].FullName
	}

	// Alphabetical by name: Jotunn, PlantEverything, Sailing.
	want := []string{"ValheimModding-Jotunn", "Advize-PlantEverything", "Smoothbrain-Sailing"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paged in order %v, want %v", seen, want)
	}
}
