package instance

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorldPathRejectsAnythingOutsideWorlds is B5, and it runs before any filesystem call —
// WorldPath is a pure function, so a rejected name never reaches the disk to be checked.
func TestWorldPathRejectsAnythingOutsideWorlds(t *testing.T) {
	const dataDir = "/srv/valmin/instances/abc"

	rejected := []string{
		"..",
		"../server/valheim_server.x86_64",
		"../../../etc/passwd",
		"sub/../../escape",       // clean() resolves this to an escape; a ".." scan alone catches it, a naive prefix check does not
		"a/b/../../../../etc/pw", // several levels, only the last of which escapes
		"",                       // the worlds directory itself is not a file
		".",
		"/",
	}
	for _, name := range rejected {
		if got, err := WorldPath(dataDir, name); !errors.Is(err, ErrOutsideWorlds) {
			t.Errorf("WorldPath(%q) = %q, %v; want ErrOutsideWorlds", name, got, err)
		}
	}

	accepted := map[string]string{
		"adminlist.txt":            "/srv/valmin/instances/abc/worlds/adminlist.txt",
		"worlds_local/Dedi.db":     "/srv/valmin/instances/abc/worlds/worlds_local/Dedi.db",
		"sub/../adminlist.txt":     "/srv/valmin/instances/abc/worlds/adminlist.txt",
		"/etc/passwd":              "/srv/valmin/instances/abc/worlds/etc/passwd", // an absolute name is rooted, not honoured
		"worlds_local/../bans.txt": "/srv/valmin/instances/abc/worlds/bans.txt",
	}
	for name, want := range accepted {
		got, err := WorldPath(dataDir, name)
		if err != nil || got != want {
			t.Errorf("WorldPath(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

// TestWriteWorldFileIsAtomic covers the guarantee 06 §4 asks for: the temp file lands in the
// same directory as its target (a rename is only atomic within one filesystem, and worlds/
// is a bind mount), and nothing partial is ever visible under the real name.
func TestWriteWorldFileIsAtomic(t *testing.T) {
	dataDir := t.TempDir()

	if err := WriteWorldFile(dataDir, "adminlist.txt", []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorldFile(dataDir, "adminlist.txt")
	if err != nil || string(got) != "first\n" {
		t.Fatalf("read back = %q, %v; want \"first\\n\"", got, err)
	}

	if err := WriteWorldFile(dataDir, "adminlist.txt", []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	got, _ = ReadWorldFile(dataDir, "adminlist.txt")
	if string(got) != "second\n" {
		t.Errorf("overwrite = %q, want \"second\\n\"", got)
	}

	// No staging file survives a successful write; one that did would eventually be
	// mistaken for content by anything walking worlds/.
	entries, err := os.ReadDir(WorldsDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".valmin-") {
			t.Errorf("staging file %s left behind after a successful write", e.Name())
		}
	}
}

// TestWriteWorldFileLeavesTheOriginalIntactWhenItCannotPublish is the interrupted-write
// case. The rename is made to fail by making the target a directory, which is the only
// in-process way to reach the failure a crash between fsync and rename produces: the
// original bytes are what a reader still sees, and the partial write never appears under
// the real name.
func TestWriteWorldFileLeavesTheOriginalIntactWhenItCannotPublish(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteWorldFile(dataDir, "worlds_local/keep.txt", []byte("original\n")); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should go: os.Rename onto it fails, after the write and
	// the fsync have already happened.
	blocked := filepath.Join(WorldsDir(dataDir), "blocked.txt")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WriteWorldFile(dataDir, "blocked.txt", []byte("replacement\n")); err == nil {
		t.Fatal("a write that cannot be published reported success")
	}
	if got, _ := ReadWorldFile(dataDir, "worlds_local/keep.txt"); string(got) != "original\n" {
		t.Errorf("an unrelated file changed: %q", got)
	}
	entries, err := os.ReadDir(WorldsDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".valmin-") {
			t.Errorf("staging file %s left behind after a failed write", e.Name())
		}
	}
}

// TestReadWorldFileTreatsAMissingFileAsEmpty: the game creates none of 03 §4's lists until
// something writes one, so an absent list must read the same as an empty one.
func TestReadWorldFileTreatsAMissingFileAsEmpty(t *testing.T) {
	got, err := ReadWorldFile(t.TempDir(), "adminlist.txt")
	if err != nil || got != nil {
		t.Errorf("ReadWorldFile of a missing file = %q, %v; want nil, nil", got, err)
	}
}

// TestOnlyTheAuditedHelperWritesFiles is B4 with teeth. 06 §4 requires every write under
// worlds/ to go through one helper; a grep cannot tell which path a given os.WriteFile
// targets, so this is deliberately blunter — no package may reach the filesystem's write
// side at all except the three that have a stated reason to. Adding a fourth means adding a
// line here, which is the review moment this test exists to force.
func TestOnlyTheAuditedHelperWritesFiles(t *testing.T) {
	// path -> why it is allowed to write without going through WriteWorldFile.
	allowed := map[string]string{
		"internal/instance/worlds.go":  "the audited helper itself",
		"internal/config/verify.go":    "10 §1.2's host-root token and the data.root writability probe, both outside any instance",
		"internal/crypto/masterkey.go": "10 §3.1's master key at ${data.root}/secret.key, which predates every instance",
		"internal/backup/archive.go":   "writes archives *out of* worlds/ into ${data.root}/backups/; it only ever reads the worlds tree",
		"internal/api/worlds.go":       "streams an upload into ${data.root}/staging/ (11 §8.3); the move *into* worlds/ still goes through WriteWorldFile",
	}
	writers := map[string]bool{"WriteFile": true, "Create": true, "CreateTemp": true, "OpenFile": true}

	root := repoRoot(t)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if _, ok := allowed[rel]; ok {
			return nil
		}

		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !writers[sel.Sel.Name] {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
				t.Errorf("%s calls os.%s directly; every write under worlds/ goes through "+
					"instance.WriteWorldFile (B4, 06 §4). If this one genuinely cannot, add it to "+
					"this test's allowlist with the reason.", rel, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// repoRoot walks up from the package directory to the module root, so the test does not
// depend on where `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}
