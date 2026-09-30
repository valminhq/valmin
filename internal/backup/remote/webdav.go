package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/notify"
)

type WebDAVBackend struct {
	URL            string
	Username       string
	Password       string
	Folder         string
	AllowedPrivate []netip.Prefix
	// Transport is injected by tests; production always uses address-checked dialing.
	Transport http.RoundTripper
}

func (b *WebDAVBackend) Validate() error {
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (b.Folder != "" && !ValidKey(b.Folder)) {
		return ErrConfiguration
	}
	return nil
}

func (b *WebDAVBackend) allowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if notify.AllowedAddress(addr) == nil {
		return true
	}
	if !addr.IsPrivate() {
		return false
	}
	for _, prefix := range b.AllowedPrivate {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (b *WebDAVBackend) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrConfiguration
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, &Failure{Message: "remote hostname could not be resolved", Temporary: true}
	}
	for _, addr := range addresses {
		if !b.allowed(addr) {
			return nil, ErrConfiguration
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, addr := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, &Failure{Message: "remote connection failed", Temporary: true}
}

func (b *WebDAVBackend) request(
	ctx context.Context,
	method, key string,
	body io.Reader,
	size int64,
) (*http.Response, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if !ValidKey(key) {
		return nil, ErrConfiguration
	}
	u, _ := url.Parse(b.URL)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path.Join(b.Folder, key)
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, ErrConfiguration
	}
	req.ContentLength = size
	req.SetBasicAuth(b.Username, b.Password)
	req.Header.Set("User-Agent", "valmin")
	transport := b.Transport
	if transport == nil {
		transport = &http.Transport{
			DialContext: b.dial, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute, DisableKeepAlives: true,
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrConfiguration) {
			return nil, ErrConfiguration
		}
		return nil, &Failure{Message: "remote request failed", Temporary: true}
	}
	return resp, nil
}

func closeResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
	_ = resp.Body.Close()
}

func responseError(status int) error {
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	return &Failure{
		Message:   fmt.Sprintf("remote server returned HTTP %d", status),
		Temporary: status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500,
	}
}

func (b *WebDAVBackend) mkdir(ctx context.Context, key string) error {
	// The base URL is an existing collection; only descendants are created.
	copyBackend := *b
	copyBackend.Folder = ""
	parent := path.Dir(path.Join(b.Folder, key))
	if parent == "." {
		return nil
	}
	parts := strings.Split(parent, "/")
	for i := range parts {
		resp, err := copyBackend.request(ctx, "MKCOL", strings.Join(parts[:i+1], "/"), nil, 0)
		if err != nil {
			return err
		}
		closeResponse(resp)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
			return responseError(resp.StatusCode)
		}
	}
	return nil
}

func (b *WebDAVBackend) Put(ctx context.Context, key, localPath string) (Object, error) {
	if err := b.Validate(); err != nil {
		return Object{}, err
	}
	if !ValidKey(key) {
		return Object{}, ErrConfiguration
	}
	f, err := os.Open(localPath) //nolint:gosec // Source comes from the internal backup catalogue.
	if err != nil {
		return Object{}, &Failure{Message: "local archive is unavailable"}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Object{}, &Failure{Message: "local archive is unavailable"}
	}
	if err := b.mkdir(ctx, key); err != nil {
		return Object{}, err
	}
	resp, err := b.request(ctx, http.MethodPut, key, f, info.Size())
	if err != nil {
		return Object{}, err
	}
	closeResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Object{}, responseError(resp.StatusCode)
	}
	return Object{Ref: ObjectRef{Key: key}, SizeBytes: info.Size()}, nil
}

func (b *WebDAVBackend) Stat(ctx context.Context, ref ObjectRef) (Object, error) {
	resp, err := b.request(ctx, http.MethodHead, ref.Key, nil, 0)
	if err != nil {
		return Object{}, err
	}
	closeResponse(resp)
	if resp.StatusCode != http.StatusOK {
		return Object{}, responseError(resp.StatusCode)
	}
	if resp.ContentLength < 0 {
		return Object{}, &Failure{Message: "remote server did not report object size"}
	}
	return Object{Ref: ref, SizeBytes: resp.ContentLength}, nil
}

func (b *WebDAVBackend) Delete(ctx context.Context, ref ObjectRef) error {
	resp, err := b.request(ctx, http.MethodDelete, ref.Key, nil, 0)
	if err != nil {
		return err
	}
	closeResponse(resp)
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return responseError(resp.StatusCode)
}
