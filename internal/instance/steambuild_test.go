package instance

import (
	"os"
	"path/filepath"
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
				got, err = PublicBuildID("Loading Steam API...OK.\n" + string(data) + "\nUnloading Steam API...OK.\n")
			} else {
				got, err = ManifestBuildID(string(data))
			}
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("build = %q, %v; want %q", got, err, tc.want)
			}
		})
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

func TestQueryPublicBuildRejectsBadOutputAndCleansUp(t *testing.T) {
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
			if id, err := QueryPublicBuild(t.Context(), fake, "steamcmd"); err == nil || id != "" {
				t.Fatalf("build=%q error=%v", id, err)
			}
			all, err := fake.List(t.Context(), nil)
			if err != nil || len(all) != 0 {
				t.Fatalf("throwaway leaked: %d %v", len(all), err)
			}
		})
	}
}
