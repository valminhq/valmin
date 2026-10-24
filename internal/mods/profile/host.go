package profile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// dataPrefix starts the text a host stores: this line, then the archive in base64.
const dataPrefix = "#r2modman\n"

var keyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseKey reports whether s, ignoring case and surrounding space, is a profile code: a UUID in
// its canonical form. It returns the lowercase form a host expects.
func ParseKey(s string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(s))
	return key, keyPattern.MatchString(key)
}

// Errors a host reports, for the caller to tell apart.
var (
	ErrNotFound  = errors.New("profile code not found")
	ErrTooLarge  = errors.New("profile is larger than the host accepts")
	ErrMalformed = errors.New("host returned data that is not a profile")
)

// Host is a registry that stores profile codes for r2modman and Gale.
type Host struct {
	BaseURL string
	Client  *http.Client
}

// Fetch downloads the archive stored under key, which ParseKey has accepted. maxBytes bounds the stored text.
func (h *Host) Fetch(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	resp, err := h.do(ctx, http.MethodGet, "get/"+key+"/", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch profile %s: unexpected status %s", key, resp.Status)
	}
	text, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", key, err)
	}
	if int64(len(text)) > maxBytes {
		return nil, ErrTooLarge
	}
	encoded, ok := strings.CutPrefix(string(text), dataPrefix)
	if !ok {
		return nil, ErrMalformed
	}
	archive, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return archive, nil
}

// Upload stores the archive and returns the code that fetches it.
func (h *Host) Upload(ctx context.Context, archive []byte) (string, error) {
	body := dataPrefix + base64.StdEncoding.EncodeToString(archive)
	resp, err := h.do(ctx, http.MethodPost, "create/", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return "", ErrTooLarge
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", fmt.Errorf("upload profile: unexpected status %s", resp.Status)
	}
	var reply struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&reply); err != nil {
		return "", fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	key, ok := ParseKey(reply.Key)
	if !ok {
		return "", fmt.Errorf("%w: key %q", ErrMalformed, reply.Key)
	}
	return key, nil
}

func (h *Host) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	url := strings.TrimSuffix(h.BaseURL, "/") + "/api/experimental/legacyprofile/" + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "valmin")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", h.BaseURL, err)
	}
	return resp, nil
}
