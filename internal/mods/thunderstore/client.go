// Package thunderstore is a client for Thunderstore's v1 community package listing, and for
// the Thunderstore-compatible hosts that serve the same response shape (03 §6.1). It imports
// neither store nor api (CLAUDE.md §5) — a breaking API change is a one-file fix.
//
// Specification: 03 §6.1, 03 §6.2, 03 §6.3.
package thunderstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// community is fixed: the panel is Valheim-only (01 §4, N1 — a second game is a fork
// decision), so unlike BaseURL this is not a configuration key.
const community = "valheim"

// Where a host serves the v1 listing. Thunderstore scopes it to a community; a compatible
// host that serves only one game puts it at the API root, and asking such a host for the
// community path is a 404. Neither is a configuration key: the path is a property of a
// registry, and the set of registries is closed in Go.
const (
	communityListingPath = "/c/" + community + "/api/v1/package/"
	bareListingPath      = "/api/v1/package/"
)

// ErrSchemaMismatch is returned when the response decoded as valid JSON but not one package in
// it carried a full_name, so a field rename upstream fails loudly rather than silently
// populating an index of empty rows.
var ErrSchemaMismatch = errors.New("thunderstore: response did not decode into any recognisable package")

// Client is a Thunderstore v1 API client, scoped to one community's package listing
// (03 §6.1). It holds no state between calls — the ETag a caller wants to send is kept by
// the caller, in kv (10 §4.2).
type Client struct {
	BaseURL    string
	HTTPClient *http.Client

	// listingPath is where this host serves the v1 listing. Empty means the community path,
	// so a Client built as a struct literal still addresses Thunderstore.
	listingPath string
}

// syncTimeout bounds one listing request end to end, and reachTimeout the Reachable
// probe, which transfers no body.
const (
	syncTimeout  = 5 * time.Minute
	reachTimeout = 10 * time.Second
)

// New builds a Client against a Thunderstore host, which serves the listing under its
// community path (10 §1.1 — baseURL is overridable for tests and fixtures).
func New(baseURL string) *Client {
	return &Client{
		BaseURL:     baseURL,
		HTTPClient:  &http.Client{Timeout: syncTimeout},
		listingPath: communityListingPath,
	}
}

// NewBare builds a Client against a Thunderstore-compatible host that serves the v1 listing
// at the API root, with no community segment.
func NewBare(baseURL string) *Client {
	return &Client{
		BaseURL:     baseURL,
		HTTPClient:  &http.Client{Timeout: syncTimeout},
		listingPath: bareListingPath,
	}
}

// Reachable probes the listing endpoint Sync uses and reports why it did not answer.
// A non-empty etag is sent as If-None-Match, making the usual answer an empty 304. The
// body is closed unread: a 200 carries the whole listing.
func (c *Client) Reachable(ctx context.Context, etag string) error {
	ctx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listingURL(), http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.listingURL(), err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing is read from it
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("reach %s: unexpected status %s", c.listingURL(), resp.Status)
	}
	return nil
}

// Result is what one Sync call learned.
type Result struct {
	ETag        string
	NotModified bool
	Count       int
}

// Sync streams the community package listing, calling onPackage once per decoded package
// without holding the whole response in memory: the v1 listing returns every package with full
// version history in one response. If onPackage returns an error, Sync stops reading and returns
// it, so a batch-flush failure partway through does not keep downloading.
//
// A non-empty etag is sent as If-None-Match; a 304 short-circuits with Result.NotModified and
// calls onPackage for nothing. A host that sends no validators at all answers 200 every time
// and leaves Result.ETag empty, which is that host's normal and not a fault (03 §6.1).
func (c *Client) Sync(ctx context.Context, etag string, onPackage func(Package) error) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.listingURL(), http.NoBody)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.client().Do(req)
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

// decodeStream reads a top-level JSON array one element at a time, so memory stays bounded
// regardless of how large the community listing grows.
//
// A package with no full_name is never handed to onPackage: full_name is mod_packages' primary
// key (04 §2), so upserting one anyway would collide every such row under one empty key instead
// of surfacing as the "named < count" mismatch Sync reports once decoding finishes.
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

// client is HTTPClient, or a default for a Client built as a literal.
func (c *Client) client() *http.Client {
	if c.HTTPClient == nil {
		return http.DefaultClient
	}
	return c.HTTPClient
}

func (c *Client) listingURL() string {
	path := c.listingPath
	if path == "" {
		path = communityListingPath
	}
	return strings.TrimRight(c.BaseURL, "/") + path
}
