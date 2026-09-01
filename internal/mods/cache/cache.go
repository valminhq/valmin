package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// MaxDownloadBytes bounds one cached zip, independent of anything the response or the
// caller's declared size claims — the same "never trust the declared size alone" rule
// extract.go's writeEntry applies to a zip entry, applied here to the zip itself. A var,
// not a const, so a test can shrink it rather than downloading real gigabytes.
var MaxDownloadBytes = int64(512 << 20) // 512 MiB

// ErrTooLarge is a download that exceeded MaxDownloadBytes, or a completed download whose
// size disagreed with the caller's declared size.
var ErrTooLarge = errors.New("cache: download exceeds size limit")

// ErrInvalidIdent is an ident that could not safely become a cache filename. ident
// originates from Thunderstore's API response (03 §6.2's Namespace-Name-Version) with no
// format validation anywhere upstream of this package — the same B5 discipline extract.go
// applies to a zip entry's name applies here to a string that becomes a path component.
var ErrInvalidIdent = errors.New("cache: invalid ident")

// Root is 02 §3's cache/thunderstore/ under data.root.
func Root(dataRoot string) string {
	return filepath.Join(dataRoot, "cache", "thunderstore")
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

	mu      sync.Mutex
	byIdent map[string]*inflight
}

// New builds a Cache rooted at root, which need not exist yet.
func New(root string) *Cache {
	return &Cache{root: root, httpClient: http.DefaultClient, byIdent: map[string]*inflight{}}
}

// path is where ident's zip would live once cached. Rejects any ident that is not a
// single path component — no "/" or "\", regardless of platform, and not empty — so an
// ident built from a malformed or hostile Thunderstore response cannot resolve outside
// root the way "../../worlds/x" would if joined without this check.
func (c *Cache) path(ident string) (string, error) {
	if ident == "" || strings.ContainsAny(ident, `/\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidIdent, ident)
	}
	return filepath.Join(c.root, ident+".zip"), nil
}

// Get returns the local path to ident's zip, downloading from downloadURL if it is not
// already cached. declaredSize is mod_versions.file_size — a cross-check against the
// completed download, never the size limit itself; pass 0 if unknown.
//
// `↯` Concurrent calls for the same ident, within this process, converge on one download:
// the second caller waits on the first's result rather than starting a second GET. This
// is process-local only — the panel is single-daemon-per-database (C7, ADR-031), so that
// is the whole scope a download race can occur in.
func (c *Cache) Get(ctx context.Context, ident, downloadURL string, declaredSize int64) (string, error) {
	final, err := c.path(ident)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}

	c.mu.Lock()
	if f, ok := c.byIdent[ident]; ok {
		c.mu.Unlock()
		<-f.done
		return f.path, f.err
	}
	f := &inflight{done: make(chan struct{})}
	c.byIdent[ident] = f
	c.mu.Unlock()

	f.path, f.err = c.download(ctx, ident, downloadURL, declaredSize, final)
	close(f.done)

	c.mu.Lock()
	delete(c.byIdent, ident)
	c.mu.Unlock()

	return f.path, f.err
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
	out, err := os.OpenFile( //nolint:gosec // ident and root are panel-built, never a request value
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

	n, copyErr := io.CopyN(out, resp.Body, MaxDownloadBytes+1)
	switch {
	case copyErr == nil:
		return "", fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, ident, MaxDownloadBytes)
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

// Sweep removes every *.part file under root, unconditionally (12 §9.4's rule for the
// backup archive, applied to the same shape of artefact): a panel killed mid-download
// leaves one behind, and no catalogue entry — nothing else — ever points at it, so nothing
// is lost by deleting it. Call once at daemon startup, before anything calls Get.
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
