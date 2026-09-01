package thunderstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// community is fixed: the panel is Valheim-only (01 §4, N1 — a second game is a fork
// decision), so unlike BaseURL this is not a configuration key.
const community = "valheim"

// ErrSchemaMismatch is returned when the response decoded as valid JSON but not one
// package in it carried a full_name — the shape CLAUDE.md §9 warns against: it succeeds,
// logs nothing, and does nothing. A field rename upstream must fail loudly rather than
// silently populate an index of empty rows.
var ErrSchemaMismatch = errors.New("thunderstore: response did not decode into any recognisable package")

// Client is a Thunderstore v1 API client, scoped to one community's package listing
// (03 §6.1). It holds no state between calls — the ETag a caller wants to send is kept by
// the caller, in kv (10 §4.2).
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// New builds a Client against baseURL (config.Thunderstore.BaseURL — overridable for
// tests and fixtures, 10 §1.1).
func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTPClient: http.DefaultClient}
}

// Result is what one Sync call learned.
type Result struct {
	ETag        string
	NotModified bool
	Count       int
}

// Sync streams the community package listing, calling onPackage once per decoded package
// without ever holding the whole response in memory at once — 03 §6.1's `↯`: the v1
// listing returns every package with full version history in one response, which for
// Valheim measured 162 MB across ~10,500 packages (1 Sep 2026). If onPackage returns an
// error, Sync stops reading and returns it — a batch-flush failure partway through must
// not keep downloading.
//
// A non-empty etag is sent as If-None-Match; a 304 short-circuits with Result.NotModified
// and calls onPackage for nothing.
func (c *Client) Sync(ctx context.Context, etag string, onPackage func(Package) error) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listingURL(), http.NoBody)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", c.listingURL(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return Result{ETag: etag, NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("thunderstore: unexpected status %s", resp.Status)
	}

	count, named, err := decodeStream(resp.Body, onPackage)
	if err != nil {
		return Result{}, err
	}
	if named < count {
		return Result{}, fmt.Errorf("%w: %d of %d decoded packages had no full_name",
			ErrSchemaMismatch, count-named, count)
	}

	return Result{ETag: resp.Header.Get("ETag"), Count: count}, nil
}

// decodeStream reads a top-level JSON array one element at a time. json.Decoder never
// holds more than one Package's bytes at once, which is what keeps memory bounded
// regardless of how large the community listing grows — proven, not just claimed, by
// TestDecodeStreamProcessesOneElementAtATime.
//
// `↯` A package with no full_name is never handed to onPackage. full_name is
// mod_packages' primary key (04 §2), so a caller that upserted one anyway would collide
// every such row under the same empty key — a schema drift affecting only some entries
// would then silently clobber several packages down to one, instead of surfacing as the
// "named < count" mismatch Sync reports once decoding finishes.
func decodeStream(r io.Reader, onPackage func(Package) error) (count, named int, err error) {
	dec := json.NewDecoder(r)
	if _, err := dec.Token(); err != nil { // the opening '['
		return 0, 0, fmt.Errorf("read listing: %w", err)
	}
	for dec.More() {
		var pkg Package
		if err := dec.Decode(&pkg); err != nil {
			return count, named, fmt.Errorf("decode package %d: %w", count, err)
		}
		count++
		if pkg.FullName == "" {
			continue
		}
		named++
		if err := onPackage(pkg); err != nil {
			return count, named, err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing ']'
		return count, named, fmt.Errorf("read listing: %w", err)
	}
	return count, named, nil
}

func (c *Client) listingURL() string {
	return strings.TrimRight(c.BaseURL, "/") + "/c/" + community + "/api/v1/package/"
}
