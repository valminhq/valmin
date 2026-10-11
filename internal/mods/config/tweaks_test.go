package config

import (
	"os"
	"path/filepath"
	"testing"
)

func readTweaksFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tweaks", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestTweaks asserts which settings count as changed and how they are written.
func TestTweaks(t *testing.T) {
	cases := []struct {
		name          string
		orig, current []byte
		want          string
		kept, secrets int
	}{
		{
			"changed, update-added and secret settings",
			readTweaksFixture(t, "settings.orig"), readTweaksFixture(t, "settings.current"),
			string(readTweaksFixture(t, "settings.tweaks")), 3, 1,
		},
		{
			"an empty original falls back to declared defaults",
			nil, readTweaksFixture(t, "settings.current"),
			"[General]\nDamageMultiplier = 1.5\nRange = 25\n[Logging.Console]\nEnabled = true\n", 3, 1,
		},
		{"nothing changed", []byte("[A]\nx = 1\n"), []byte("[A]\nx = 1\n"), "", 0, 0},
		{
			"settings before any section come first without a header",
			[]byte("top = 1\n[A.B]\nx = 1\n"), []byte("top = 2\n[A.B]\nx = 2\n"),
			"top = 2\n[A.B]\nx = 2\n", 2, 0,
		},
		{"a setting without a declared default is kept", nil, []byte("[A]\nx = 1\n"), "[A]\nx = 1\n", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, kept, secrets := Tweaks(tc.orig, tc.current)
			if string(got) != tc.want {
				t.Errorf("Tweaks =\n%s\nwant\n%s", got, tc.want)
			}
			if kept != tc.kept || secrets != tc.secrets {
				t.Errorf("kept, secrets = %d, %d, want %d, %d", kept, secrets, tc.kept, tc.secrets)
			}
		})
	}
}

// TestMerge asserts settings land in place when present and are added otherwise, with the rest
// of the file untouched.
func TestMerge(t *testing.T) {
	cases := []struct {
		name, base, over, want string
	}{
		{"no file yet", "", "[A]\nx = 1\n", "[A]\nx = 1\n"},
		{
			"an existing setting changes in place",
			"## Comment\n[A]\n# Default value: 1\nx  =  1\r\ny = 2\r\n", "[A]\nx = 5\n",
			"## Comment\n[A]\n# Default value: 1\nx  =  5\r\ny = 2\r\n",
		},
		{
			"missing settings are added under their sections",
			"[A]\nx = 1\n[B]\ny = 2", "[A]\nx = 1\nz = 3\n[C]\nw = 4\n",
			"[A]\nx = 1\n[B]\ny = 2\n[A]\nz = 3\n[C]\nw = 4\n",
		},
		{"a sectionless setting goes first", "[A]\nx = 1\n", "top = 1\n", "top = 1\n[A]\nx = 1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Merge([]byte(tc.base), []byte(tc.over)))
			if got != tc.want {
				t.Errorf("Merge =\n%q\nwant\n%q", got, tc.want)
			}
			for _, s := range Parse([]byte(tc.over)).Settings() {
				if v, ok := Parse([]byte(got)).Get(s.Section, s.Key); !ok || v != s.Value {
					t.Errorf("[%s] %s = %q, %v after merge, want %q", s.Section, s.Key, v, ok, s.Value)
				}
			}
		})
	}
}

// TestKeepCopies asserts the backup follows every write while the original is taken once.
func TestKeepCopies(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, content := range []string{"", "first", "second"} {
		if err := KeepCopies(root, "a.cfg", []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"a.cfg" + OriginalSuffix: "", "a.cfg" + BackupSuffix: "second"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", name, got, err, want)
		}
	}
}
