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

func TestModVersionDependenciesReadsAndDecodes(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	if err := db.UpsertModPackages(ctx, nil, []ModVersion{{
		FullName: "ValheimModding-Jotunn", Version: "2.29.2",
		DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2333"]`,
	}}); err != nil {
		t.Fatal(err)
	}

	deps, ok, err := db.ModVersionDependencies(ctx, "ValheimModding-Jotunn", "2.29.2")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(deps) != 1 || deps[0] != "denikson-BepInExPack_Valheim-5.4.2333" {
		t.Errorf("deps = %v, ok = %v", deps, ok)
	}
}

// TestModVersionDependenciesMissingIsFalseNotError distinguishes "version not in the
// index" from "version in the index with zero dependencies" — the resolver's
// UnresolvedError depends on telling these apart.
func TestModVersionDependenciesMissingIsFalseNotError(t *testing.T) {
	db := open(t)
	_, ok, err := db.ModVersionDependencies(t.Context(), "Nobody-Home", "1.0.0")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true for a version not in the index, want false")
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
		// The two rows relevance ranking exists for. Both would outrank Sailing on a
		// search for "sailing" if downloads alone decided the order: one matches only in
		// its description and is 5,000× more downloaded, the other matches the name
		// exactly and is abandoned.
		{
			FullName: "Popular-Everything", Namespace: "Popular", Name: "Everything",
			Description: "Adds sailing, farming and mining", Downloads: 999999,
			CategoriesJSON: `["Mods"]`,
		},
		{
			FullName: "Ghost-Sailing", Namespace: "Ghost", Name: "Sailing",
			Description: "Abandoned", Downloads: 50000, IsDeprecated: true,
			CategoriesJSON: `["Mods"]`,
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
// existing: paging through with limit=1 must see every row exactly once, in the order the
// unpaged query would have produced.
func TestSearchModPackagesPaginatesWithoutDuplicateOrSkip(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	var seen []string
	afterSortKey, afterFullName := "", ""
	for {
		page, err := db.SearchModPackages(t.Context(), "", "", afterSortKey, afterFullName, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].FullName)
		afterSortKey, afterFullName = page[0].SearchSortKey, page[0].FullName
	}

	// With no query every row ranks equally, so this is the popularity order — which is
	// what browsing the catalogue cold should show (ADR-114). Ghost-Sailing is last
	// despite 50,000 downloads because it is deprecated.
	want := []string{
		"Popular-Everything", "ValheimModding-Jotunn", "Advize-PlantEverything",
		"Smoothbrain-Sailing", "Ghost-Sailing",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paged in order %v, want %v", seen, want)
	}
}

// TestSearchModPackagesRanksNameMatchesOverDescription is the regression test for the
// catalogue being unusable at real scale (ADR-114). A search for a package's own name must
// return that package first, whatever else mentions the word in passing.
func TestSearchModPackagesRanksNameMatchesOverDescription(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)

	got, err := db.SearchModPackages(t.Context(), "sailing", "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}

	order := make([]string, 0, len(got))
	for _, p := range got {
		order = append(order, p.FullName)
	}
	// Smoothbrain-Sailing has 200 downloads and is last of the three on popularity alone.
	// It is first because its *name* is the term: an exact name match outranks a deprecated
	// exact match, which outranks a description-only match with five thousand times the
	// downloads.
	want := []string{"Smoothbrain-Sailing", "Ghost-Sailing", "Popular-Everything"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("ranked %v, want %v", order, want)
	}
}

// TestSearchModPackagesRanksPrefixOverSubstring separates the two middle tiers, which the
// corpus above cannot: "sail" is a prefix of Sailing and appears mid-word in Unsailable.
func TestSearchModPackagesRanksPrefixOverSubstring(t *testing.T) {
	db := open(t)
	seedSearchCorpus(t, db)
	err := db.UpsertModPackages(t.Context(), []ModPackage{{
		FullName: "Someone-Unsailable", Namespace: "Someone", Name: "Unsailable",
		Description: "Unrelated", Downloads: 888888, CategoriesJSON: `["Mods"]`,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.SearchModPackages(t.Context(), "sail", "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].FullName != "Smoothbrain-Sailing" {
		t.Fatalf("first result is %+v, want Smoothbrain-Sailing: a prefix beats a substring", got)
	}
}
