package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/errcode"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

// buildTarGz returns a gzipped tar holding entries, a regular file unless typeflag says otherwise.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: flag, Mode: 0o644}
		if flag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if flag == tar.TypeSymlink {
			hdr.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stagedFiles lists every regular file under dir, slash-separated and sorted.
func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// The archive a backup writes is the tree under worlds/, so a world sits under worlds_local/.
// Staging has to reduce every shape to what the folder upload produces: the file, or the one
// directory above it, and nothing else.
func TestStageArchive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []tarEntry
		want    []string
	}{
		{
			name: "a pair",
			entries: []tarEntry{
				{name: "worlds_local/", typeflag: tar.TypeDir},
				{name: "worlds_local/Dvalin.db", body: "db"},
				{name: "worlds_local/Dvalin.fwl", body: "fwl"},
			},
			want: []string{"worlds_local/Dvalin.db", "worlds_local/Dvalin.fwl"},
		},
		{
			name: "a directory world keeps its directory",
			entries: []tarEntry{
				{name: "worlds_local/Dvalin/_main.3.db2", body: "db"},
				{name: "worlds_local/Dvalin/_main.3.fwl2", body: "fwl"},
				{name: "worlds_local/Dvalin/_main.3.chunk", body: "c"},
			},
			want: []string{"Dvalin/_main.3.chunk", "Dvalin/_main.3.db2", "Dvalin/_main.3.fwl2"},
		},
		{
			name: "rolling saves and other files are left out",
			entries: []tarEntry{
				{name: "worlds_local/Dvalin.db", body: "db"},
				{name: "worlds_local/Dvalin.fwl", body: "fwl"},
				{name: "worlds_local/Dvalin_backup_auto-20260903073824.db", body: "x"},
				{name: "worlds_local/Dvalin_backup_auto-20260903073824.fwl", body: "x"},
				{name: "worlds_local/notes.txt", body: "x"},
			},
			want: []string{"worlds_local/Dvalin.db", "worlds_local/Dvalin.fwl"},
		},
		{
			name: "traversal and absolute names stay inside staging",
			entries: []tarEntry{
				{name: "../../escape.db", body: "x"},
				{name: "/etc/absolute.fwl", body: "x"},
			},
			want: []string{"escape.db", "etc/absolute.fwl"},
		},
		{
			name: "links and devices are skipped",
			entries: []tarEntry{
				{name: "worlds_local/Link.db", typeflag: tar.TypeSymlink},
				{name: "worlds_local/Dev.db", typeflag: tar.TypeChar},
				{name: "worlds_local/Dvalin.db", body: "db"},
			},
			want: []string{"worlds_local/Dvalin.db"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			staging := t.TempDir()
			data := buildTarGz(t, tc.entries)
			_, err := stageArchive(bytes.NewReader(data), staging, newUploadBudget(1<<20, 16))
			if err != nil {
				t.Fatalf("stageArchive: %v", err)
			}
			if got := stagedFiles(t, staging); !slices.Equal(got, tc.want) {
				t.Errorf("staged %v, want %v", got, tc.want)
			}
		})
	}
}

// A download cut short still ends in a valid tar prefix; only gzip's trailer says it is not
// the whole archive, so staging must read to the end of the gzip stream.
func TestStageArchiveRefusesATruncatedArchive(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{
		{name: "worlds_local/Dvalin.db", body: string(make([]byte, 4096))},
		{name: "worlds_local/Dvalin.fwl", body: "fwl"},
	})
	for _, cut := range []int{len(data) - 4, len(data) / 2} {
		_, err := stageArchive(bytes.NewReader(data[:cut]), t.TempDir(), newUploadBudget(1<<20, 16))
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) || apiErr.Code != errcode.InvalidParameter {
			t.Errorf("archive cut at %d of %d bytes: err = %v, want invalid_parameter", cut, len(data), err)
		}
	}
}

// The budget bounds what the archive expands to, not what it compresses to.
func TestStageArchiveHonoursTheBudget(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{
		{name: "worlds_local/Dvalin.db", body: string(make([]byte, 64<<10))},
		{name: "worlds_local/Dvalin.fwl", body: "fwl"},
	})

	cases := []struct {
		name   string
		budget *uploadBudget
	}{
		{"bytes", newUploadBudget(1<<10, 16)},
		{"entries", newUploadBudget(1<<20, 1)},
	}
	for _, tc := range cases {
		_, err := stageArchive(bytes.NewReader(data), t.TempDir(), tc.budget)
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) || apiErr.Code != errcode.PayloadTooLarge {
			t.Errorf("%s: err = %v, want payload_too_large", tc.name, err)
		}
	}
}

// An archive with no world in it is not an error here: the import job reports "no world was
// found" in the operator's words.
func TestStageArchiveWithNothingInIt(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{{name: "readme.txt", body: "hi"}})
	n, err := stageArchive(bytes.NewReader(data), t.TempDir(), newUploadBudget(1<<20, 16))
	if err != nil || n != 0 {
		t.Errorf("stageArchive = %d, %v, want 0, nil", n, err)
	}
}

// A panel backup uploaded under its own name reaches stageArchive through the same multipart
// path as a zip or a folder, whatever case the browser reports the extension in.
func TestStageUploadExpandsABackupArchive(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{
		{name: "worlds_local/Dvalin.db", body: "db"},
		{name: "worlds_local/Dvalin.fwl", body: "fwl"},
	})
	for _, filename := range []string{"backup.tar.gz", "BACKUP.TGZ"} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())

		staging := t.TempDir()
		if err := stageUploadWithLimits(req, staging, 1<<20, 16); err != nil {
			t.Fatalf("%s: stageUploadWithLimits: %v", filename, err)
		}
		want := []string{"worlds_local/Dvalin.db", "worlds_local/Dvalin.fwl"}
		if got := stagedFiles(t, staging); !slices.Equal(got, want) {
			t.Errorf("%s: staged %v, want %v", filename, got, want)
		}
	}
}

// Nothing legitimate follows a tar's end marker but block padding, so a long tail is refused
// instead of being decompressed to look for the gzip trailer.
func TestStageArchiveRefusesALongTail(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "Dvalin.db", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("db")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := gz.Write(make([]byte, 2*archiveTailLimit)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := stageArchive(&buf, t.TempDir(), newUploadBudget(1<<20, 16))
	if err == nil {
		t.Error("an archive with a long tail after its end marker staged without error")
	}
}

// An entry the filter skips is still inflated to reach the next header, and it costs the
// budget nothing, so the archive as a whole is capped by what it expands to.
func TestStageArchiveCapsWhatSkippedEntriesExpandTo(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{
		{name: "worlds_local/notes.txt", body: string(make([]byte, 64<<10))},
		{name: "worlds_local/Dvalin.db", body: "db"},
	})

	_, err := stageArchiveCapped(bytes.NewReader(data), t.TempDir(), newUploadBudget(1<<20, 16), 4<<10)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != errcode.InvalidParameter {
		t.Errorf("err = %v, want invalid_parameter", err)
	}

	// The same archive is whole when the cap allows what it expands to.
	if _, err := stageArchiveCapped(bytes.NewReader(data), t.TempDir(), newUploadBudget(1<<20, 16), 1<<20); err != nil {
		t.Errorf("under the cap: %v", err)
	}
}

// Too many files is a property of the upload, not of the disk, so it stays payload_too_large
// even when free space had cut the byte budget.
func TestDiskLimitedLeavesTheFileCountAlone(t *testing.T) {
	t.Parallel()
	data := buildTarGz(t, []tarEntry{
		{name: "worlds_local/Dvalin.db", body: "db"},
		{name: "worlds_local/Dvalin.fwl", body: "fwl"},
	})
	const diskLimitedBudget = 1 << 20
	_, err := stageArchive(bytes.NewReader(data), t.TempDir(), newUploadBudget(diskLimitedBudget, 1))
	var apiErr *apierr.Error
	if err = diskLimited(err, diskLimitedBudget); !errors.As(err, &apiErr) || apiErr.Code != errcode.PayloadTooLarge {
		t.Errorf("too many files under a disk-limited budget = %v, want payload_too_large", err)
	}

	_, err = stageArchive(bytes.NewReader(data), t.TempDir(), newUploadBudget(1, 16))
	if err = diskLimited(err, 1); !errors.As(err, &apiErr) || apiErr.Code != errcode.InsufficientDisk {
		t.Errorf("too many bytes under a disk-limited budget = %v, want insufficient_disk", err)
	}
}
