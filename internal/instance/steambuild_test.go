package instance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
)

func TestSteamBuildRecords(t *testing.T) {
	for _, tc := range []struct {
		file   string
		public bool
		want   string
	}{
		{"app-info.vdf", true, "21981590"},
		{"appmanifest.acf", false, "21981590"},
		{"no-public.vdf", true, ""},
		{"duplicate.vdf", true, ""},
		{"truncated.vdf", true, ""},
		{"wrong-app.acf", false, ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "steam", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if tc.public {
				var branches []SteamBranch
				branches, err = SteamBranches(
					"Loading Steam API...OK.\n" + string(data) + "\nUnloading Steam API...OK.\n",
				)
				if err == nil {
					got = branches[0].BuildID
				}
			} else {
				got, err = ManifestBuildID(string(data))
			}
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("build = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestSteamBranchesListsEveryAnonymousBranch(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "steam", "app-info.vdf"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := SteamBranches(string(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []SteamBranch{
		{Name: "public", BuildID: "21981590"},
		{Name: "default_old", BuildID: "20460518", Description: "Previous stable"},
		{Name: "default_preal", BuildID: "20221240", Description: "Last stable build before Ashlands"},
		{Name: "default_prebw", BuildID: "20221628", Description: "Last stable build before Bog Witch"},
		{Name: "default_precta", BuildID: "20221863", Description: "Last stable build before Call to Arms"},
		{Name: "default_preml", BuildID: "20222098", Description: "Last stable build before Mistlands"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("branches = %+v\nwant %+v", got, want)
	}
}

func TestSteamBranchesSkipsUnusableBranches(t *testing.T) {
	out := `"896660"
{ "depots" { "branches" {
	"public" { "buildid" "100" }
	"locked" { "buildid" "200" "pwdrequired" "1" }
	"broken" { "buildid" "latest" }
	"+quit" { "buildid" "300" }
} } }`
	got, err := SteamBranches(out)
	if err != nil || len(got) != 1 || got[0].Name != "public" {
		t.Fatalf("branches = %+v, %v", got, err)
	}
}

func TestManifestBranch(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"appmanifest.acf", "public"},
		{"appmanifest-branch.acf", "default_preml"},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", "steam", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ManifestBranch(string(data)); err != nil || got != tc.want {
			t.Errorf("%s: branch = %q, %v; want %q", tc.file, got, err, tc.want)
		}
	}
}

func TestBranchArgumentValidation(t *testing.T) {
	for _, tc := range []struct {
		value    string
		name, pw bool
	}{
		{"public-test", true, true},
		{"default_pre1_0", true, true},
		{"yesimadebackups", true, true},
		{"p@ss!word", false, true},
		{"", false, false},
		{"+quit", false, false},
		{"-beta", false, false},
		{"_x", false, true},
		{"two words", false, false},
		{`quo"te`, false, false},
		{strings.Repeat("a", 65), false, false},
	} {
		if got := ValidBranchName(tc.value) == nil; got != tc.name {
			t.Errorf("ValidBranchName(%q) = %v, want %v", tc.value, got, tc.name)
		}
		if got := ValidBranchPassword(tc.value) == nil; got != tc.pw {
			t.Errorf("ValidBranchPassword(%q) = %v, want %v", tc.value, got, tc.pw)
		}
	}
}

func TestSteamBuildRejectsInvalidIDs(t *testing.T) {
	for _, id := range []string{"", "0", "latest", "-1", "1.5", "18446744073709551616"} {
		if _, err := validSteamBuild(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
	if _, err := ManifestBuildID(strings.Repeat("x", steamMetadataLimit+1)); err == nil {
		t.Error("accepted oversized metadata")
	}
}

func TestInstalledBuildStaysInsideInstance(t *testing.T) {
	dir := t.TempDir()
	if _, err := InstalledBuildID(dir); err == nil {
		t.Fatal("accepted missing manifest")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "server")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstalledBuildID(dir); err == nil {
		t.Fatal("followed escaping symlink")
	}
}

func TestQueryBuildsRejectsBadOutputAndCleansUp(t *testing.T) {
	for _, name := range []string{"no-public.vdf", "truncated.vdf", "oversized"} {
		t.Run(name, func(t *testing.T) {
			var output string
			if name == "oversized" {
				output = strings.Repeat("x", steamMetadataLimit+1)
			} else {
				data, err := os.ReadFile(filepath.Join("testdata", "steam", name))
				if err != nil {
					t.Fatal(err)
				}
				output = string(data)
			}
			fake := runtime.NewFake()
			fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout(output); c.Exit(0) }
			if got, err := QueryBuilds(t.Context(), fake, "steamcmd", ""); err == nil || got.BuildID != "" {
				t.Fatalf("build=%q error=%v", got.BuildID, err)
			}
			all, err := fake.List(t.Context(), nil)
			if err != nil || len(all) != 0 {
				t.Fatalf("throwaway leaked: %d %v", len(all), err)
			}
		})
	}
}
