package extract

import (
	"archive/zip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipFile builds a zip at a fresh temp path from the given entries and returns its path.
// entries whose header is already fully formed (SetMode called) are written as-is, so a
// test can hand-craft an unsafe entry the same way a hostile package would.
func zipFile(t *testing.T, entries ...func(*zip.Writer) error) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, add := range entries {
		if err := add(zw); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// file writes a regular entry with the stdlib's default (no Unix mode bits at all — the
// shape a Windows-built zip produces).
func file(name, content string) func(*zip.Writer) error {
	return func(zw *zip.Writer) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}
}

// unixFile writes a regular entry with an explicit Unix permission, so a test can prove
// the archive's own mode is discarded rather than honoured.
func unixFile(name, content string, mode fs.FileMode) func(*zip.Writer) error {
	return func(zw *zip.Writer) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}
}

// symlink writes an entry whose Unix mode bits mark it as a symlink — a zip's only way to
// represent one — with target as its "content" the way a real archiver would.
func symlink(name, target string) func(*zip.Writer) error {
	return func(zw *zip.Writer) error {
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(fs.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(target))
		return err
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	// Enough "../" segments to clear any temp-dir depth regardless of the machine running
	// the test.
	evil := strings.Repeat("../", 12) + "evil.txt"
	zp := zipFile(t, file(evil, "pwned"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Extract(%q) = %v, want ErrUnsafePath", evil, err)
	}
	assertEmpty(t, dest)
}

// TestExtractNormalisesBeforeValidating is F3: a Windows-built zip (Tekla-AutoRepair in
// the real corpus) stores backslash-separated names. If normalisation ran *after* the
// traversal check, "..\\..\\evil.txt" would look like one harmless filename and pass —
// this test is written so it would pass under that (wrong) ordering, which is exactly why
// it is not enough on its own; TestExtractRejectsPathTraversal above proves the forward-
// slash form is caught, and this one proves the same string with backslashes is too.
func TestExtractNormalisesBeforeValidating(t *testing.T) {
	evil := strings.Repeat(`..\`, 12) + `evil.txt`
	zp := zipFile(t, file(evil, "pwned"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Extract(%q) = %v, want ErrUnsafePath (normalisation must precede validation)", evil, err)
	}
	assertEmpty(t, dest)
}

func TestExtractRejectsAbsolutePath(t *testing.T) {
	zp := zipFile(t, file("/etc/passwd", "pwned"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Extract of an absolute entry = %v, want ErrUnsafePath", err)
	}
	assertEmpty(t, dest)
}

func TestExtractRejectsSymlinks(t *testing.T) {
	zp := zipFile(t, symlink("innocuous.txt", "/etc/passwd"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrUnsafeType) {
		t.Fatalf("Extract of a symlink entry = %v, want ErrUnsafeType", err)
	}
	assertEmpty(t, dest)
}

// TestExtractIsAllOrNothing proves the hostile entry does not have to be first: a
// legitimate file ahead of it in the archive must not survive on disk either, because the
// whole archive is validated before anything is written.
func TestExtractIsAllOrNothing(t *testing.T) {
	zp := zipFile(t,
		file("manifest.json", `{"name":"Innocuous"}`),
		file("plugins/Innocuous.dll", "not really a dll"),
		symlink("plugins/evil", "/etc/passwd"),
	)
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrUnsafeType) {
		t.Fatalf("Extract = %v, want ErrUnsafeType", err)
	}
	assertEmpty(t, dest)
}

func TestExtractEnforcesTheEntryCap(t *testing.T) {
	orig := MaxEntries
	MaxEntries = 3
	t.Cleanup(func() { MaxEntries = orig })

	zp := zipFile(t, file("a", "1"), file("b", "2"), file("c", "3"), file("d", "4"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrLimit) {
		t.Fatalf("Extract over the entry cap = %v, want ErrLimit", err)
	}
	assertEmpty(t, dest)
}

func TestExtractEnforcesTheTotalSizeCap(t *testing.T) {
	orig := MaxTotalUncompressedBytes
	MaxTotalUncompressedBytes = 10
	t.Cleanup(func() { MaxTotalUncompressedBytes = orig })

	zp := zipFile(t, file("a", "0123456789"), file("b", "x"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrLimit) {
		t.Fatalf("Extract over the total-size cap = %v, want ErrLimit", err)
	}
	// The cap is exceeded once entry b's byte is counted — nothing, including entry a,
	// must have been written yet: the failure is caught in the validation pass, before the
	// write pass a single one of these caps is meant to guard.
	assertEmpty(t, dest)
}

func TestExtractEnforcesThePerEntryCap(t *testing.T) {
	orig := MaxEntryUncompressedBytes
	MaxEntryUncompressedBytes = 4
	t.Cleanup(func() { MaxEntryUncompressedBytes = orig })

	zp := zipFile(t, file("huge.dll", "way too much content for the cap"))
	dest := t.TempDir()

	if err := Extract(zp, dest); !errors.Is(err, ErrLimit) {
		t.Fatalf("Extract over the per-entry cap = %v, want ErrLimit", err)
	}
	assertEmpty(t, dest)
}

// TestExtractNormalisesModes is 08 §2.1 against an archive that claims 0777 — M0's own
// finding, that cp -a faithfully reproduces an archive's drwxrwxrwx, is exactly what this
// package must not do.
func TestExtractNormalisesModes(t *testing.T) {
	zp := zipFile(t, unixFile("plugins/Mod.dll", "binary content", 0o777))
	dest := t.TempDir()

	if err := Extract(zp, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dest, "plugins", "Mod.dll"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != fileMode {
		t.Errorf("file mode = %o, want %o (the archive's declared 0777 must be discarded)", got, fileMode)
	}

	di, err := os.Stat(filepath.Join(dest, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode() & (fs.ModePerm | fs.ModeSetgid); got != dirMode {
		t.Errorf("directory mode = %o, want %o (setgid, 2775)", got, dirMode)
	}
}

// TestExtractPreservesContentByteForByte is F2: two real packages in the corpus
// (ValheimModding-Jotunn, SpikeHimself-XPortal) ship a manifest.json beginning with a
// UTF-8 BOM. Extraction has no business interpreting the bytes as text, and this proves it
// doesn't — the BOM survives, which is what lets a JSON decoder elsewhere strip it
// deliberately rather than have it silently eaten or mis-encoded in transit.
func TestExtractPreservesContentByteForByte(t *testing.T) {
	bom := "\xEF\xBB\xBF"
	manifest := bom + `{"name":"Jotunn","dependencies":[]}`
	zp := zipFile(t, file("manifest.json", manifest))
	dest := t.TempDir()

	if err := Extract(zp, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != manifest {
		t.Errorf("extracted manifest.json = %q, want %q (byte-identical, BOM included)", got, manifest)
	}
}

func TestExtractPlacesNestedDirectoriesAndFiles(t *testing.T) {
	zp := zipFile(t,
		file("BepInEx/plugins/Jotunn.dll", "dll bytes"),
		file("BepInEx/config/Jotunn.cfg", "cfg bytes"),
		file("icon.png", "png bytes"),
	)
	dest := t.TempDir()

	if err := Extract(zp, dest); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"BepInEx/plugins/Jotunn.dll": "dll bytes",
		"BepInEx/config/Jotunn.cfg":  "cfg bytes",
		"icon.png":                   "png bytes",
	} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func assertEmpty(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("destination is not empty after a rejected archive: %v", entries)
	}
}
