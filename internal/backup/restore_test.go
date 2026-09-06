package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one member of a hand-built archive, so a test can write a name and a type the
// panel's own Archive would never produce.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// buildArchive writes entries as a gzipped tar and returns its path.
func buildArchive(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name: e.name, Mode: 0o664, Typeflag: typeflag,
			Size: int64(len(e.body)), Linkname: e.linkname,
		}
		if typeflag == tar.TypeDir {
			hdr.Mode, hdr.Size = 0o775, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Asserts Extract takes the prefixed subtree and only that subtree, with the prefix stripped.
func TestExtractTakesOnlyThePrefixedSubtree(t *testing.T) {
	archive := buildArchive(t, []tarEntry{
		{name: "worlds_local/", typeflag: tar.TypeDir},
		{name: "worlds_local/World.db", body: "save"},
		{name: "worlds_local/nested/extra.txt", body: "deeper"},
		{name: "adminlist.txt", body: "not part of the world"},
	})
	dest := filepath.Join(t.TempDir(), "worlds_local.new")

	if err := Extract(archive, "worlds_local", dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "World.db")); got != "save" {
		t.Errorf("World.db = %q, want %q", got, "save")
	}
	if got := readFile(t, filepath.Join(dest, "nested", "extra.txt")); got != "deeper" {
		t.Errorf("nested/extra.txt = %q, want %q", got, "deeper")
	}
	if _, err := os.Stat(filepath.Join(dest, "adminlist.txt")); !os.IsNotExist(err) {
		t.Error("an entry outside the prefix was extracted")
	}
}

// Asserts an entry that would resolve outside the destination is refused, whichever way it
// spells the escape (B5).
func TestExtractRefusesAnEntryThatEscapesTheDestination(t *testing.T) {
	for _, name := range []string{
		"worlds_local/../../escaped.db",
		"worlds_local/a/../../../escaped.db",
	} {
		t.Run(name, func(t *testing.T) {
			archive := buildArchive(t, []tarEntry{{name: name, body: "hostile"}})
			dest := filepath.Join(t.TempDir(), "staged")

			err := Extract(archive, "worlds_local", dest)
			if !errors.Is(err, ErrUnsafeEntry) {
				t.Fatalf("Extract = %v, want ErrUnsafeEntry", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.db")); !os.IsNotExist(err) {
				t.Error("the entry landed outside the destination anyway")
			}
		})
	}
}

// Asserts a symlink entry is refused rather than followed: a link inside the staged world
// would be renamed into worlds/ and read by the server (B5).
func TestExtractRefusesASymlinkEntry(t *testing.T) {
	archive := buildArchive(t, []tarEntry{
		{name: "worlds_local/World.db", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})

	err := Extract(archive, "worlds_local", filepath.Join(t.TempDir(), "staged"))
	if !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("Extract = %v, want ErrUnsafeEntry", err)
	}
}

// Asserts a truncated archive is refused rather than extracted as far as it reads.
func TestExtractRefusesATruncatedArchive(t *testing.T) {
	archive := buildArchive(t, []tarEntry{
		{name: "worlds_local/World.db", body: strings.Repeat("save data ", 500)},
	})
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Extract(archive, "worlds_local", filepath.Join(t.TempDir(), "staged")); err == nil {
		t.Fatal("Extract accepted a truncated archive")
	}
}

// worldsWith builds a worlds directory holding the named directories, each with a marker file
// naming it, so a swap can be told apart from a copy.
func worldsWith(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// marker reports which directory ended up under live, by the marker the fixture wrote into it.
func marker(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name, "marker"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Asserts Swap publishes the staged world and leaves neither of the other two behind.
func TestSwapPublishesTheStagedWorld(t *testing.T) {
	root := worldsWith(t, "worlds_local", "worlds_local.new")

	if err := Swap(filepath.Join(root, "worlds_local")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := marker(t, root, "worlds_local"); got != "worlds_local.new" {
		t.Errorf("worlds_local holds %q, want the staged world", got)
	}
	for _, leftover := range []string{"worlds_local.new", "worlds_local.old"} {
		if _, err := os.Stat(filepath.Join(root, leftover)); !os.IsNotExist(err) {
			t.Errorf("%s survived the swap", leftover)
		}
	}
}

// Asserts a restore into an instance that has no world yet still publishes: there is nothing
// to set aside, which is not a failure.
func TestSwapPublishesWhenThereIsNoWorldToDisplace(t *testing.T) {
	root := worldsWith(t, "worlds_local.new")

	if err := Swap(filepath.Join(root, "worlds_local")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := marker(t, root, "worlds_local"); got != "worlds_local.new" {
		t.Errorf("worlds_local holds %q, want the staged world", got)
	}
}

// Asserts every state the two renames can be interrupted in resolves to exactly one world
// under the live name, and to the right one (12 §9.4).
func TestRecoverSwapResolvesEveryInterruptedState(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
	}{
		{"before either rename", []string{"worlds_local", "worlds_local.new"}, "worlds_local"},
		{"between the renames", []string{"worlds_local.old", "worlds_local.new"}, "worlds_local.new"},
		{"after the second rename", []string{"worlds_local", "worlds_local.old"}, "worlds_local"},
		{"staged world lost", []string{"worlds_local.old"}, "worlds_local.old"},
		{"nothing was in progress", []string{"worlds_local"}, "worlds_local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := worldsWith(t, tt.present...)

			if _, err := RecoverSwap(filepath.Join(root, "worlds_local")); err != nil {
				t.Fatalf("RecoverSwap: %v", err)
			}
			if got := marker(t, root, "worlds_local"); got != tt.want {
				t.Errorf("worlds_local holds %q, want %q", got, tt.want)
			}
			for _, leftover := range []string{"worlds_local.new", "worlds_local.old"} {
				if _, err := os.Stat(filepath.Join(root, leftover)); !os.IsNotExist(err) {
					t.Errorf("%s survived recovery", leftover)
				}
			}
		})
	}
}

// Asserts recovery over a worlds directory with no restore in flight touches nothing and
// reports no error, since the sweep runs for every interrupted restore regardless of how far
// it got.
func TestRecoverSwapIsANoOpWhenNothingIsThere(t *testing.T) {
	root := t.TempDir()

	action, err := RecoverSwap(filepath.Join(root, "worlds_local"))
	if err != nil {
		t.Fatalf("RecoverSwap: %v", err)
	}
	if action == "" {
		t.Error("RecoverSwap reported nothing about what it did")
	}
}
