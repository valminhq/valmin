// Package cache is the content-addressed Thunderstore zip cache (03 §6.1, 03 §6.2,
// 02 §3): a package version is downloaded at most once per host and shared across every
// instance that installs it. It imports neither store nor api (CLAUDE.md §5) — the
// caller supplies the download URL and the declared size, both already known to whoever
// resolved the closure.
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/idleread"
	"github.com/valminhq/valmin/internal/mods/source"
)

// MaxDownloadBytes bounds one cached zip, independent of any declared size, the same rule
// extract.go's writeEntry applies to a zip entry.
const MaxDownloadBytes = int64(512 << 20) // 512 MiB

// Download time limits. Connecting and receiving response headers are bounded outright; the
// body may take as long as it keeps arriving, and stalls after idleTimeout without a byte.
const (
	dialTimeout   = 15 * time.Second
	headerTimeout = 30 * time.Second
	idleTimeout   = time.Minute
)

// ErrStalled is a download whose body stopped arriving.
var ErrStalled = errors.New("cache: download stalled")

// ErrTooLarge is a download that exceeded its size limit, or a completed download whose
// size disagreed with the caller's declared size.
var ErrTooLarge = errors.New("cache: download exceeds size limit")

// ErrInvalidIdent is an ident that could not safely become a cache filename. ident originates
// from Thunderstore's API response (03 §6.2) with no format validation upstream, so it gets the
// same B5 discipline extract.go applies to a zip entry's name.
var ErrInvalidIdent = errors.New("cache: invalid ident")

// Root is 02 §3's cache/thunderstore/ under data.root, the path Thunderstore's zips have
// always used.
func Root(dataRoot string) string {
	return RootFor(dataRoot, source.Thunderstore)
}

// RootFor is one registry's cache directory under data.root. Registries never share one:
// two of them can serve different bytes under a single package-version ident, and a shared
// directory would hand one registry's zip to an install of the other's (B14).
func RootFor(dataRoot string, s source.Source) string {
	return filepath.Join(dataRoot, "cache", s.String())
}

// inflight is one download in progress, shared by every concurrent Get for the same ident.
type inflight struct {
	done chan struct{}
	path string
	err  error
}

// Cache is the zip cache over one root directory. The zero value is not usable; build one
// with New.
type Cache struct {
	root       string
	httpClient *http.Client
	maxBytes   int64
	idle       time.Duration

	mu      sync.Mutex
	byIdent map[string]*inflight
}

// New builds a Cache rooted at root, which need not exist yet.
func New(root string) *Cache {
	dialer := &net.Dialer{Timeout: dialTimeout}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: headerTimeout,
	}
	return &Cache{
		root: root, httpClient: &http.Client{Transport: transport},
		maxBytes: MaxDownloadBytes, idle: idleTimeout, byIdent: map[string]*inflight{},
	}
}

// path is where ident's zip would live once cached. Rejects any ident that is not a single path
// component, so a malformed or hostile registry response, or a package name in a request, cannot
// resolve outside root.
func (c *Cache) path(ident string) (string, error) {
	if ident == "" || strings.Contains(ident, "..") || strings.ContainsAny(ident, `/\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidIdent, ident)
	}
	return filepath.Join(c.root, ident+".zip"), nil
}

// Has reports whether ident's zip is cached.
func (c *Cache) Has(ident string) bool {
	path, err := c.path(ident)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// Get returns the local path to ident's zip, downloading from downloadURL if not already
// cached. declaredSize is mod_versions.file_size, a cross-check against the completed download
// rather than the size limit; pass 0 if unknown.
//
// Concurrent calls for the same ident converge on one download within this process, which is
// the whole scope a race can occur in: the panel is single-daemon-per-database (C7, ADR-031).
// A caller waiting on another's download follows its own context, and starts its own download
// when the other one was cancelled.
func (c *Cache) Get(ctx context.Context, ident, downloadURL string, declaredSize int64) (string, error) {
	for {
		path, shared, err := c.get(ctx, ident, downloadURL, declaredSize)
		if shared && isContextError(err) && ctx.Err() == nil {
			continue
		}
		return path, err
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// get is one attempt of Get; shared reports whether the result came from another caller's
// download.
func (c *Cache) get(
	ctx context.Context,
	ident, downloadURL string,
	declaredSize int64,
) (path string, shared bool, err error) {
	final, err := c.path(ident)
	if err != nil {
		return "", false, err
	}

	// A hit is checked and touched under the lock Prune holds, so a zip handed out here is
	// never one a concurrent prune already judged unused.
	c.mu.Lock()
	if info, err := os.Stat(final); err == nil {
		touch(final, info)
		c.mu.Unlock()
		return final, false, nil
	}
	if f, ok := c.byIdent[ident]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.path, true, f.err
		case <-ctx.Done():
			return "", false, fmt.Errorf("wait for %s: %w", ident, ctx.Err())
		}
	}
	f := &inflight{done: make(chan struct{})}
	c.byIdent[ident] = f
	c.mu.Unlock()

	f.path, f.err = c.download(ctx, ident, downloadURL, declaredSize, final)
	close(f.done)

	c.mu.Lock()
	delete(c.byIdent, ident)
	c.mu.Unlock()

	return f.path, false, f.err
}

// download fetches downloadURL to <final>.part, verifies it, and publishes it by rename —
// the same discipline internal/backup.Archive uses: fsync before rename, and a half-written
// file is never visible under its final name.
func (c *Cache) download(
	ctx context.Context,
	ident, downloadURL string,
	declaredSize int64,
	final string,
) (string, error) {
	if err := fsutil.MkdirAllExact(c.root); err != nil {
		return "", fmt.Errorf("create cache root: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build request for %s: %w", ident, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", ident, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cache: %s: unexpected status %s", ident, resp.Status)
	}

	part := final + ".part"
	out, err := os.OpenFile(
		part,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		fsutil.FileMode,
	)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", part, err)
	}
	published := false
	defer func() {
		_ = out.Close()
		if !published {
			_ = os.Remove(part)
		}
	}()

	body := idleread.New(resp.Body, c.idle, cancel)
	defer body.Stop()
	n, copyErr := io.CopyN(out, body, c.maxBytes+1)
	switch {
	case copyErr == nil:
		return "", fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, ident, c.maxBytes)
	case body.Stalled():
		return "", fmt.Errorf("%w: %s sent nothing for %s", ErrStalled, ident, c.idle)
	case !errors.Is(copyErr, io.EOF):
		return "", fmt.Errorf("download %s: %w", ident, copyErr)
	}
	if declaredSize > 0 && n != declaredSize {
		return "", fmt.Errorf(
			"%w: %s downloaded %d bytes, mod_versions declared %d",
			ErrTooLarge,
			ident,
			n,
			declaredSize,
		)
	}

	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("flush %s: %w", part, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", part, err)
	}
	if err := os.Rename(part, final); err != nil {
		return "", fmt.Errorf("publish %s: %w", ident, err)
	}
	published = true

	return final, nil
}

// touchInterval bounds how often a cache hit refreshes a zip's times. Half the panel's one-hour
// prune grace period, it leaves a zip at least half an hour of protection after any hit while
// sparing an inode write on most installs.
const touchInterval = 30 * time.Minute

// touch marks a cached zip as just used, so a prune's grace period covers whoever was handed it.
// A failure only costs that protection, never the hit.
func touch(path string, info os.FileInfo) {
	if time.Since(info.ModTime()) < touchInterval {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// Entry is one file under the cache root: a published zip, or the leftover of a download.
type Entry struct {
	Ident   string
	Partial bool
	fsutil.Usage
}

// Entries lists the zips and partial downloads under the cache root in name order. A missing
// root is an empty cache.
func (c *Cache) Entries() ([]Entry, error) {
	dirents, err := os.ReadDir(c.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list cache %s: %w", c.root, err)
	}
	var entries []Entry
	for _, d := range dirents {
		ident, partial, ok := parseName(d.Name())
		if !ok || !d.Type().IsRegular() {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, Entry{Ident: ident, Partial: partial, Usage: fsutil.StatUsage(info)})
	}
	return entries, nil
}

// parseName splits a cache filename into its ident and whether it is a partial download.
func parseName(name string) (ident string, partial, ok bool) {
	if ident, found := strings.CutSuffix(name, ".zip.part"); found {
		return ident, true, ident != ""
	}
	if ident, found := strings.CutSuffix(name, ".zip"); found {
		return ident, false, ident != ""
	}
	return "", false, false
}

// Prune removes the entries remove selects and returns them. It holds the cache lock
// throughout, so no Get reuses or publishes a zip between the listing and the removal, and it
// never touches the files of a download in flight.
func (c *Cache) Prune(remove func(Entry) bool) ([]Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.Entries()
	if err != nil {
		return nil, err
	}
	var removed []Entry
	for _, e := range entries {
		if _, downloading := c.byIdent[e.Ident]; downloading || !remove(e) {
			continue
		}
		name := e.Ident + ".zip"
		if e.Partial {
			name += ".part"
		}
		if err := os.Remove(filepath.Join(c.root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("remove cached %s: %w", name, err)
		}
		removed = append(removed, e)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, fsutil.SyncDir(c.root)
}

// Sweep removes every *.part file under root, unconditionally: a panel killed mid-download
// leaves one behind that no catalogue entry ever points at (12 §9.4). Call once at daemon
// startup, before anything calls Get.
func Sweep(root string) error {
	matches, err := filepath.Glob(filepath.Join(root, "*.part"))
	if err != nil {
		return fmt.Errorf("sweep cache %s: %w", root, err)
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sweep cache: remove %s: %w", m, err)
		}
	}
	return nil
}
