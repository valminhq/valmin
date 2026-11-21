package manager

import (
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
)

// TestToStoreRowsIgnoresCategoryOrder asserts two listings of one package that differ only in
// category order store the same row, so a reorder alone never rewrites it.
func TestToStoreRowsIgnoresCategoryOrder(t *testing.T) {
	t.Parallel()
	rows := make([]string, 0, 2)
	for _, categories := range [][]string{{"Tweaks", "Libraries", "Client-side"}, {"Client-side", "Tweaks", "Libraries"}} {
		pkg := thunderstore.Package{FullName: "a-b", Owner: "a", Name: "b", Categories: categories}
		row, _, err := ToStoreRows(&pkg, source.Thunderstore)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row.CategoriesJSON)
	}
	if rows[0] != rows[1] {
		t.Errorf("CategoriesJSON = %s and %s, want one value for one set", rows[0], rows[1])
	}
	if want := `["Client-side","Libraries","Tweaks"]`; rows[0] != want {
		t.Errorf("CategoriesJSON = %s, want %s", rows[0], want)
	}

	empty := thunderstore.Package{FullName: "a-b", Owner: "a", Name: "b", Categories: []string{}}
	row, _, err := ToStoreRows(&empty, source.Thunderstore)
	if err != nil {
		t.Fatal(err)
	}
	if row.CategoriesJSON != "[]" {
		t.Errorf("CategoriesJSON of no categories = %s, want []", row.CategoriesJSON)
	}
}
