// Package templaterepo reads a template repository: a .tar.gz archive whose single top-level
// directory holds templates/<slug>/template.yaml, each naming a share code. Everything else in
// the archive is ignored.
package templaterepo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/valminhq/valmin/internal/sharecode"
)

// Official is the archive of Valmin's own template repository, which every panel offers.
const Official = "https://github.com/valminhq/valmin-presets/archive/refs/heads/main.tar.gz"

// Template is one usable entry of a repository.
type Template struct {
	Slug        string
	Name        string
	Description string
	Code        string
	Mods        int
}

// Problem is a template.yaml that cannot be offered, with the reason worded for its author.
type Problem struct {
	Path   string
	Detail string
}

// Result is one fetch of a repository. NotModified means the archive still matches ETag and
// nothing else is set.
type Result struct {
	ETag        string
	NotModified bool
	Templates   []Template
	Problems    []Problem
}

// limits bounds one fetch. extracted caps the decompressed archive, which bounds a gzip bomb.
type limits struct {
	extracted   int64
	file        int64
	code        int64
	templates   int
	name        int
	description int
}

var defaultLimits = limits{
	extracted:   256 << 20,
	file:        1 << 20,
	code:        8 << 20,
	templates:   500,
	name:        120,
	description: 2000,
}

var (
	errTooLarge = errors.New("the archive is larger than this panel reads")
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Fetch downloads the archive at url and reads its templates. A non-empty etag is sent as
// If-None-Match. One broken template.yaml is a Problem; only a failed download or an unreadable
// archive is an error.
func Fetch(ctx context.Context, client *http.Client, url, etag string) (Result, error) {
	return fetch(ctx, client, url, etag, defaultLimits)
}

func fetch(ctx context.Context, client *http.Client, url, etag string, lim limits) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return Result{ETag: etag, NotModified: true}, nil
	case http.StatusOK:
	default:
		return Result{}, fmt.Errorf("download %s: %s", url, resp.Status)
	}

	res, err := read(resp.Body, lim)
	if err != nil {
		return Result{}, err
	}
	res.ETag = resp.Header.Get("ETag")
	return res, nil
}

// read walks a .tar.gz stream and parses every templates/<slug>/template.yaml in it.
func read(r io.Reader, lim limits) (Result, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Result{}, fmt.Errorf("read archive: %w", err)
	}
	tr := tar.NewReader(&capReader{r: gz, n: lim.extracted + 1})
	var res Result
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return Result{}, errTooLarge
			}
			return Result{}, fmt.Errorf("read archive: %w", err)
		}
		slug, ok := templatePath(hdr.Name)
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if len(res.Templates)+len(res.Problems) >= lim.templates {
			return Result{}, fmt.Errorf("the archive holds more than %d templates", lim.templates)
		}
		if err := readEntry(tr, hdr.Size, slug, lim, &res); err != nil {
			return Result{}, err
		}
	}
}

// readEntry adds the template.yaml at the reader's current entry to res, as a template or a
// problem. Only a failed read of the archive itself is an error.
func readEntry(tr io.Reader, size int64, slug string, lim limits, res *Result) error {
	path := "templates/" + slug + "/template.yaml"
	if size > lim.file {
		res.Problems = append(res.Problems, Problem{Path: path, Detail: "template.yaml is larger than 1 MiB"})
		return nil
	}
	raw, err := io.ReadAll(tr)
	if errors.Is(err, errTooLarge) {
		return errTooLarge
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if t, err := parse(raw, lim); err != nil {
		res.Problems = append(res.Problems, Problem{Path: path, Detail: err.Error()})
	} else {
		t.Slug = slug
		res.Templates = append(res.Templates, t)
	}
	return nil
}

// templatePath returns the slug of a "<top>/templates/<slug>/template.yaml" entry.
func templatePath(name string) (string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[1] != "templates" || parts[3] != "template.yaml" ||
		!slugPattern.MatchString(parts[2]) {
		return "", false
	}
	return parts[2], true
}

// parse reads one template.yaml. Keys other than name, description and code are ignored.
func parse(raw []byte, lim limits) (Template, error) {
	var doc struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Code        string `yaml:"code"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Template{}, fmt.Errorf("template.yaml is not valid YAML: %w", err)
	}
	t := Template{
		Name:        strings.TrimSpace(doc.Name),
		Description: strings.TrimSpace(doc.Description),
		Code:        strings.Join(strings.Fields(doc.Code), ""),
	}
	switch {
	case t.Name == "":
		return Template{}, errors.New("name is missing")
	case utf8.RuneCountInString(t.Name) > lim.name:
		return Template{}, fmt.Errorf("name is longer than %d characters", lim.name)
	case utf8.RuneCountInString(t.Description) > lim.description:
		return Template{}, fmt.Errorf("description is longer than %d characters", lim.description)
	case t.Code == "":
		return Template{}, errors.New("code is missing")
	}
	decoded, err := sharecode.Decode(t.Code, lim.code)
	if err != nil {
		return Template{}, fmt.Errorf("code: %w", err)
	}
	t.Mods = len(decoded.Mods)
	return t, nil
}

// capReader fails with errTooLarge once more than n-1 bytes have been read.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err //nolint:wrapcheck // passes the wrapped reader's io.EOF through unchanged
}
