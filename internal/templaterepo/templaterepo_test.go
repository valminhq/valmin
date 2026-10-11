package templaterepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/sharecode"
)

// archive builds a .tar.gz holding files, keyed by path inside the archive.
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		body := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
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

func code(t *testing.T) string {
	t.Helper()
	c, err := sharecode.Encode(&sharecode.Template{
		Name: "QoL server",
		Mods: []control.ManifestMod{{FullName: "Azumatt-AzuCraftyBoxes", Version: "1.5.3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRead asserts which archive entries become templates, which become problems, and which
// limits fail the whole archive.
func TestRead(t *testing.T) {
	c := code(t)
	valid := "name: Vanilla QoL\ndescription: |\n  Less inventory juggling.\ncode: " + c + "\n"
	small := defaultLimits
	small.file, small.templates, small.extracted = 64, 2, 4096

	tests := []struct {
		name     string
		files    map[string]string
		lim      limits
		want     []Template
		problems []string
		err      string
	}{
		{
			name: "valid template beside ignored files",
			files: map[string]string{
				"repo-main/README.md":                           "# presets",
				"repo-main/templates/vanilla-qol/template.yaml": valid,
				"repo-main/templates/vanilla-qol/README.md":     "notes",
				"repo-main/templates/a/b/template.yaml":         valid,
				"repo-main/templates/Bad_Slug/template.yaml":    valid,
				"repo-main/template.yaml":                       valid,
			},
			want: []Template{{
				Slug: "vanilla-qol", Name: "Vanilla QoL", Description: "Less inventory juggling.",
				Code: c, Mods: 1,
			}},
		},
		{
			name: "folded code and unknown keys",
			files: map[string]string{
				"r/templates/qol/template.yaml": "name: QoL\ntags: [x]\ncode: >-\n  " +
					c[:20] + "\n  " + c[20:] + "\n",
			},
			want: []Template{{Slug: "qol", Name: "QoL", Code: c, Mods: 1}},
		},
		{
			name: "broken files are problems",
			files: map[string]string{
				"r/templates/no-name/template.yaml":  "code: " + c,
				"r/templates/no-code/template.yaml":  "name: x",
				"r/templates/bad-code/template.yaml": "name: x\ncode: valmin1:@@@",
				"r/templates/not-yaml/template.yaml": "name: [",
				"r/templates/ok/template.yaml":       valid,
			},
			want: []Template{{
				Slug: "ok", Name: "Vanilla QoL", Description: "Less inventory juggling.", Code: c, Mods: 1,
			}},
			problems: []string{
				"templates/bad-code/template.yaml: code: this code is damaged",
				"templates/no-code/template.yaml: code is missing",
				"templates/no-name/template.yaml: name is missing",
				"templates/not-yaml/template.yaml: template.yaml is not valid YAML",
			},
		},
		{
			name:     "file over the cap",
			files:    map[string]string{"r/templates/big/template.yaml": valid},
			lim:      small,
			problems: []string{"templates/big/template.yaml: template.yaml is larger than"},
		},
		{
			name: "too many templates",
			files: map[string]string{
				"r/templates/a/template.yaml": "name: a",
				"r/templates/b/template.yaml": "name: b",
				"r/templates/c/template.yaml": "name: c",
			},
			lim: small,
			err: "more than 2 templates",
		},
		{
			name:  "archive over the cap",
			files: map[string]string{"r/big.bin": strings.Repeat("x", 8192)},
			lim:   small,
			err:   errTooLarge.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lim := tt.lim
			if lim == (limits{}) {
				lim = defaultLimits
			}
			res, err := read(bytes.NewReader(archive(t, tt.files)), lim)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want one containing %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Templates, tt.want) {
				t.Errorf("templates = %+v\nwant %+v", res.Templates, tt.want)
			}
			if len(res.Problems) != len(tt.problems) {
				t.Fatalf("problems = %+v, want %d", res.Problems, len(tt.problems))
			}
			for i, p := range res.Problems {
				if got := p.Path + ": " + p.Detail; !strings.HasPrefix(got, tt.problems[i]) {
					t.Errorf("problem %d = %q, want prefix %q", i, got, tt.problems[i])
				}
			}
		})
	}
}

// TestFetch asserts the ETag round trip and that a failed download is an error.
func TestFetch(t *testing.T) {
	body := archive(t, map[string]string{"r/templates/qol/template.yaml": "name: QoL\ncode: " + code(t)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/main.tar.gz" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	ctx := context.Background()

	res, err := Fetch(ctx, srv.Client(), srv.URL+"/main.tar.gz", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ETag != `"v1"` || res.NotModified || len(res.Templates) != 1 {
		t.Fatalf("first fetch = %+v", res)
	}

	res, err = Fetch(ctx, srv.Client(), srv.URL+"/main.tar.gz", `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NotModified || res.ETag != `"v1"` {
		t.Fatalf("conditional fetch = %+v, want not modified", res)
	}

	if _, err := Fetch(ctx, srv.Client(), srv.URL+"/missing.tar.gz", ""); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("missing archive: err = %v, want a 404", err)
	}
}
