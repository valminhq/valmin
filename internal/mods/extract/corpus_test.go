package extract

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// TestExtractOverTheRealCorpus is the real-package half of the fixture harness (05 M2:
// "collect ~10 real Thunderstore zips"). The corpus itself is never committed (ADR-105) —
// mods are downloaded, not vendored — so this reads a local directory named by
// VALMIN_MOD_CORPUS and skips entirely when it is unset, which is the default: `make test`
// never depends on it, only a developer who has pre-downloaded packages does.
func TestExtractOverTheRealCorpus(t *testing.T) {
	dir := os.Getenv("VALMIN_MOD_CORPUS")
	if dir == "" {
		t.Skip("VALMIN_MOD_CORPUS not set; skipping the real-package extraction suite")
	}

	zips, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) == 0 {
		t.Fatalf("no .zip files in %s", dir)
	}

	for _, zp := range zips {
		zp := zp
		t.Run(filepath.Base(zp), func(t *testing.T) {
			dest := t.TempDir()
			if err := Extract(zp, dest); err != nil {
				t.Fatalf("Extract(%s) = %v", zp, err)
			}
			assertRealPackageModes(t, dest)
		})
	}
}

// assertRealPackageModes is 08 §2.1 against real archive content, not a synthetic 0777
// fixture: every one of the fifteen downloaded packages must land 0664/2775 regardless of
// what its own zip declared.
func assertRealPackageModes(t *testing.T, dest string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dest {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != fsutil.DirMode {
				t.Errorf("%s: directory mode = %o, want %o", path, got, fsutil.DirMode)
			}
			return nil
		}
		count++
		if got := info.Mode().Perm(); got != fsutil.FileMode {
			t.Errorf("%s: file mode = %o, want %o", path, got, fsutil.FileMode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("extraction produced no files")
	}
}
