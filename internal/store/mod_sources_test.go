package store

import (
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
)

// migrateTo applies every migration up to and including version v, and returns the database.
func migrateTo(t *testing.T, v int) *DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "panel.db")
	db, err := Open(t.Context(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Writer.ExecContext(t.Context(), createSchemaMigrations); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	for _, m := range migrations {
		if m.Version > v {
			break
		}
		if err := applyOne(t.Context(), db.Writer, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
	}
	return db
}

// applyVersion applies the one migration with this version to an already-migrated database.
func applyVersion(t *testing.T, db *DB, v int) {
	t.Helper()
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	for _, m := range migrations {
		if m.Version == v {
			if err := applyOne(t.Context(), db.Writer, m); err != nil {
				t.Fatalf("apply migration %d: %v", v, err)
			}
			return
		}
	}
	t.Fatalf("no migration with version %d", v)
}

// TestModSourceBackfill asserts every row written before the index carried a registry is
// attributed to Thunderstore, which is the only client that existed to write it.
func TestModSourceBackfill(t *testing.T) {
	db := migrateTo(t, 11)

	seedUser(t, db, "u1")
	seedInstance(t, db, "i1", 2456)
	exec(t, db.Writer, `
		INSERT INTO mod_packages (
			full_name, namespace, name, description, latest_version,
			downloads, rating, is_deprecated, categories, icon_url, synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"denikson-BepInExPack_Valheim", "denikson", "BepInExPack_Valheim", "loader",
		"5.4.2202", 41000000, 12, false, `["Libraries"]`, "https://example/icon.png", Now())
	exec(t, db.Writer, `
		INSERT INTO mod_versions (full_name, version, dependencies, download_url, file_size)
		VALUES (?, ?, ?, ?, ?)`,
		"denikson-BepInExPack_Valheim", "5.4.2202", `[]`, "https://example/pack.zip", 700000)
	exec(t, db.Writer, `
		INSERT INTO instance_mods (
			instance_id, full_name, version, installed_as, side, enabled,
			file_manifest, installed_at
		) VALUES (?, ?, ?, 'explicit', 'unknown', TRUE, '[]', ?)`,
		"i1", "denikson-BepInExPack_Valheim", "5.4.2202", Now())

	applyVersion(t, db, 12)

	queries := map[string]string{
		"mod_packages":  `SELECT source FROM mod_packages`,
		"mod_versions":  `SELECT source FROM mod_versions`,
		"instance_mods": `SELECT source FROM instance_mods`,
	}
	for table, query := range queries {
		var got string
		if err := db.Reader.QueryRowContext(t.Context(), query).Scan(&got); err != nil {
			t.Fatalf("read %s.source: %v", table, err)
		}
		if got != "thunderstore" {
			t.Errorf("%s.source = %q, want thunderstore", table, got)
		}
	}
}

// TestModSourceCompositeKeys asserts one full_name can be indexed from both registries at
// once, and that an instance still holds a package only once.
func TestModSourceCompositeKeys(t *testing.T) {
	db := open(t)
	seedUser(t, db, "u1")
	seedInstance(t, db, "i1", 2456)

	insertPackage := func(fullName, src string) error {
		_, err := db.Writer.ExecContext(t.Context(), `
			INSERT INTO mod_packages (
				full_name, source, namespace, name, description, latest_version,
				downloads, rating, is_deprecated, categories, icon_url, synced_at
			) VALUES (?, ?, 'denikson', 'x', '', '1.0.0', 0, 0, FALSE, '[]', '', ?)`,
			fullName, src, Now())
		return err
	}
	if err := insertPackage("a-b", "thunderstore"); err != nil {
		t.Fatalf("thunderstore row: %v", err)
	}
	if err := insertPackage("a-b", "hexium"); err != nil {
		t.Fatalf("hexium row rejected, so the key is not composite: %v", err)
	}
	if err := insertPackage("a-b", "hexium"); err == nil {
		t.Fatal("a duplicate (full_name, source) was accepted")
	}

	insertInstalled := func(src string) error {
		_, err := db.Writer.ExecContext(t.Context(), `
			INSERT INTO instance_mods (
				instance_id, full_name, source, version, installed_as, side, enabled,
				file_manifest, installed_at
			) VALUES ('i1', 'a-b', ?, '1.0.0', 'explicit', 'unknown', TRUE, '[]', ?)`,
			src, Now())
		return err
	}
	if err := insertInstalled("thunderstore"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// One instance holds a package once, from one registry: both registries place it at the
	// same path under the server root, and one file manifest describes one registry's bytes.
	if err := insertInstalled("hexium"); err == nil {
		t.Fatal("the same package was installed twice into one instance")
	}
}

// TestModVersionsKeyedBySource asserts two registries can carry the same version of one
// package with different bytes behind it.
func TestModVersionsKeyedBySource(t *testing.T) {
	db := open(t)
	insert := func(src, url string) error {
		_, err := db.Writer.ExecContext(t.Context(), `
			INSERT INTO mod_versions (full_name, source, version, dependencies, download_url, file_size)
			VALUES ('a-b', ?, '1.0.0', '[]', ?, 10)`, src, url)
		return err
	}
	if err := insert("thunderstore", "https://thunderstore.io/a.zip"); err != nil {
		t.Fatalf("thunderstore row: %v", err)
	}
	if err := insert("hexium", "https://cdn.hexium.gg/a.zip"); err != nil {
		t.Fatalf("hexium row rejected: %v", err)
	}

	var urls []string
	rows, err := db.Reader.QueryContext(t.Context(),
		`SELECT download_url FROM mod_versions WHERE full_name = 'a-b' ORDER BY source`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 {
		t.Fatalf("got %d download URLs, want 2: %v", len(urls), urls)
	}
	if urls[0] == urls[1] {
		t.Fatal("both registries resolved to one download URL")
	}
}

// seedBothRegistries indexes one package on both registries with identical popularity, so
// the two rows compute the same relevance sort key and only the row key separates them.
func seedBothRegistries(t *testing.T, db *DB) {
	t.Helper()
	all := source.All()
	packages := make([]ModPackage, 0, len(all)+1)
	for _, src := range all {
		packages = append(packages, ModPackage{
			Source: src, FullName: "denikson-BepInExPack_Valheim",
			Namespace: "denikson", Name: "BepInExPack_Valheim",
			Description: "loader", Downloads: 1000, CategoriesJSON: `["Libraries"]`,
		})
	}
	packages = append(packages, ModPackage{
		Source: source.Hexium, FullName: "Only-OnHexium",
		Namespace: "Only", Name: "OnHexium", Downloads: 1000, CategoriesJSON: `["Mods"]`,
	})
	if err := db.UpsertModPackages(t.Context(), packages, nil); err != nil {
		t.Fatal(err)
	}
}

// TestSearchPaginatesAcrossRegistriesWithoutDuplicateOrSkip is why the keyset cursor pairs
// the full name with the registry. Two registries carrying one package produce two rows with
// the same full name and the same sort key, and a cursor tiebroken on the full name alone
// would either serve one of them twice or skip it.
func TestSearchPaginatesAcrossRegistriesWithoutDuplicateOrSkip(t *testing.T) {
	db := open(t)
	seedBothRegistries(t, db)

	var seen []string
	afterSortKey, afterRowKey := "", ""
	for range 10 {
		page, err := db.SearchModPackages(t.Context(),
			&ModSearch{AfterSortKey: afterSortKey, AfterRowKey: afterRowKey, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].FullName+"@"+page[0].Source.String())
		afterSortKey, afterRowKey = page[0].SearchSortKey, page[0].SearchRowKey
	}

	want := []string{
		"Only-OnHexium@hexium",
		"denikson-BepInExPack_Valheim@hexium",
		"denikson-BepInExPack_Valheim@thunderstore",
	}
	if len(seen) != len(want) {
		t.Fatalf("paged %d rows, want %d: %v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged in order %v, want %v", seen, want)
		}
	}
}

// TestSearchFiltersByRegistry asserts a named registry narrows the result and that the zero
// value searches every one of them.
func TestSearchFiltersByRegistry(t *testing.T) {
	db := open(t)
	seedBothRegistries(t, db)

	tests := []struct {
		name string
		src  source.Source
		want int
	}{
		{"every registry", source.Source{}, 3},
		{"thunderstore only", source.Thunderstore, 1},
		{"hexium only", source.Hexium, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.SearchModPackages(t.Context(), &ModSearch{Source: tt.src, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d rows, want %d", len(got), tt.want)
			}
			for _, row := range got {
				if tt.src != (source.Source{}) && row.Source != tt.src {
					t.Errorf("%s came back from %v", row.FullName, row.Source)
				}
			}
		})
	}
}

// TestModVersionDependenciesFallsBackToTheOtherRegistry covers the closure that spans both:
// a dependency ident names no registry, so the preferred one answering nothing must not end
// the resolution.
func TestModVersionDependenciesFallsBackToTheOtherRegistry(t *testing.T) {
	db := open(t)
	if err := db.UpsertModPackages(t.Context(), nil, []ModVersion{{
		Source: source.Hexium, FullName: "Only-OnHexium", Version: "1.0.0",
		DependenciesJSON: `["denikson-BepInExPack_Valheim-5.4.2350"]`,
	}}); err != nil {
		t.Fatal(err)
	}

	deps, foundIn, ok, err := db.ModVersionDependencies(
		t.Context(), "Only-OnHexium", "1.0.0", source.Thunderstore)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a version only the other registry carries did not resolve")
	}
	if foundIn != source.Hexium {
		t.Errorf("foundIn = %v, want hexium", foundIn)
	}
	if len(deps) != 1 {
		t.Errorf("deps = %v", deps)
	}
}

// TestModVersionDownloadIsExactAboutTheRegistry is B14: two registries can serve different
// bytes under one full_name-version, so the download URL is never a preference.
func TestModVersionDownloadIsExactAboutTheRegistry(t *testing.T) {
	db := open(t)
	if err := db.UpsertModPackages(t.Context(), nil, []ModVersion{{
		Source: source.Hexium, FullName: "A-B", Version: "1.0.0",
		DependenciesJSON: `[]`, DownloadURL: "https://cdn.hexium.gg/a.zip",
	}}); err != nil {
		t.Fatal(err)
	}

	url, _, ok, err := db.ModVersionDownload(t.Context(), "A-B", "1.0.0", source.Hexium)
	if err != nil || !ok || url != "https://cdn.hexium.gg/a.zip" {
		t.Fatalf("hexium download = %q, ok = %v, err = %v", url, ok, err)
	}
	if _, _, ok, err := db.ModVersionDownload(
		t.Context(), "A-B", "1.0.0", source.Thunderstore); err != nil || ok {
		t.Fatalf("thunderstore answered for a package only hexium carries: ok = %v, err = %v", ok, err)
	}
}

func TestDependenciesRespectAllowedSources(t *testing.T) {
	db := open(t)
	if err := db.UpsertModPackages(t.Context(), nil, []ModVersion{
		{FullName: "Ns-Mod", Version: "1.0.0", Source: source.Thunderstore, DependenciesJSON: `["Ns-TS-1.0.0"]`},
		{FullName: "Ns-Mod", Version: "1.0.0", Source: source.Hexium, DependenciesJSON: `["Ns-Hex-1.0.0"]`},
		{FullName: "Ns-Mod", Version: "2.0.0", Source: source.Hexium, DependenciesJSON: `[]`},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version string
		prefer        source.Source
		allowed       []source.Source
		want          source.Source
	}{
		{"preferred", "1.0.0", source.Hexium, source.All(), source.Hexium},
		{"disabled preferred", "1.0.0", source.Hexium, []source.Source{source.Thunderstore}, source.Thunderstore},
		{"fallback", "2.0.0", source.Thunderstore, source.All(), source.Hexium},
		{"pinned registry missing version", "2.0.0", source.Thunderstore, []source.Source{source.Thunderstore}, source.Source{}},
		{"none allowed", "1.0.0", source.Hexium, nil, source.Source{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, src, ok, err := db.ModVersionDependenciesFrom(
				t.Context(), "Ns-Mod", tc.version, tc.prefer, tc.allowed,
			)
			if err != nil {
				t.Fatal(err)
			}
			if src != tc.want || ok != (tc.want != (source.Source{})) {
				t.Fatalf("source = %s, found = %v; want %s", src, ok, tc.want)
			}
			if ok && tc.version == "1.0.0" {
				want := "Ns-TS-1.0.0"
				if src == source.Hexium {
					want = "Ns-Hex-1.0.0"
				}
				if len(deps) != 1 || deps[0] != want {
					t.Errorf("dependencies = %v, want [%s]", deps, want)
				}
			}
		})
	}
}
