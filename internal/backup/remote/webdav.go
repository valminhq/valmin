package remote

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/notify"
)

// WebDAVBackend stores objects under Folder of an HTTPS WebDAV server. It dials only public
// addresses, and the private ones in AllowedPrivate.
type WebDAVBackend struct {
	URL            string
	Username       string
	Password       string
	Folder         string
	AllowedPrivate []netip.Prefix
	// Transport is injected by tests; production always uses address-checked dialing.
	Transport http.RoundTripper
}

// Validate reports ErrConfiguration for a URL that is not plain HTTPS or a folder that is not
// a valid key.
func (b *WebDAVBackend) Validate() error {
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (b.Folder != "" && !ValidKey(b.Folder)) {
		return ErrConfiguration
	}
	return nil
}

func (b *WebDAVBackend) allowed(addr netip.Addr) bool {
	return AllowedAddress(addr, b.AllowedPrivate)
}

// carrierNAT is the shared address space Tailscale and other overlay networks hand out.
var carrierNAT = netip.MustParsePrefix("100.64.0.0/10")

// AllowedAddress reports whether a remote endpoint may connect to addr: a public address, or
// a private or carrier-grade NAT one inside an operator-allowed prefix.
func AllowedAddress(addr netip.Addr, allowedPrivate []netip.Prefix) bool {
	addr = addr.Unmap()
	if notify.AllowedAddress(addr) == nil {
		return true
	}
	if !addr.IsPrivate() && !carrierNAT.Contains(addr) {
		return false
	}
	for _, prefix := range allowedPrivate {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// CheckEndpointHost resolves host and returns ErrConfiguration when any of its addresses is
// not allowed. The program that later connects resolves the name again, so records that change
// after this check are not covered.
func CheckEndpointHost(ctx context.Context, host string, allowedPrivate []netip.Prefix) error {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return &Failure{Message: "remote hostname could not be resolved", Temporary: true}
	}
	for _, addr := range addresses {
		if !AllowedAddress(addr, allowedPrivate) {
			return ErrConfiguration
		}
	}
	return nil
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
	if method == methodPropfind {
		req.Header.Set("Depth", "0")
	}
	transport := b.Transport
	if transport == nil {
		// A server may check a large upload before it answers, so a PUT waits for its reply
		// until the attempt's own deadline rather than a fixed header timeout.
		headerTimeout := 2 * time.Minute
		if method == http.MethodPut {
			headerTimeout = 0
		}
		transport = &http.Transport{
			DialContext: b.dial, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: headerTimeout, DisableKeepAlives: true,
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

// Check validates the destination's settings; WebDAV has no local configuration to inspect.
func (b *WebDAVBackend) Check(context.Context) error { return b.Validate() }

// Put uploads the file at localPath to key, creating the folders above it.
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

// Stat reads the object's size, through PROPFIND when the server sends no length on HEAD.
func (b *WebDAVBackend) Stat(ctx context.Context, ref ObjectRef) (Object, error) {
	resp, err := b.request(ctx, http.MethodHead, ref.Key, nil, 0)
	if err != nil {
		return Object{}, err
	}
	closeResponse(resp)
	if resp.StatusCode != http.StatusOK {
		return Object{}, responseError(resp.StatusCode)
	}
	if resp.ContentLength >= 0 {
		return Object{Ref: ref, SizeBytes: resp.ContentLength}, nil
	}
	size, err := b.propfindSize(ctx, ref.Key)
	if err != nil {
		return Object{}, err
	}
	return Object{Ref: ref, SizeBytes: size}, nil
}

const methodPropfind = "PROPFIND"

// maxPropfindReply bounds the multistatus body read for one object's properties.
const maxPropfindReply = 64 << 10

// propfindSize reads key's getcontentlength property, for a server whose HEAD reply carries no
// Content-Length.
func (b *WebDAVBackend) propfindSize(ctx context.Context, key string) (int64, error) {
	resp, err := b.request(ctx, methodPropfind, key, nil, 0)
	if err != nil {
		return 0, err
	}
	defer closeResponse(resp)
	if resp.StatusCode != http.StatusMultiStatus {
		return 0, responseError(resp.StatusCode)
	}
	var reply struct {
		Length string `xml:"response>propstat>prop>getcontentlength"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxPropfindReply)).Decode(&reply); err != nil {
		return 0, &Failure{Message: "remote server did not report object size"}
	}
	size, err := strconv.ParseInt(strings.TrimSpace(reply.Length), 10, 64)
	if err != nil || size < 0 {
		return 0, &Failure{Message: "remote server did not report object size"}
	}
	return size, nil
}

// Delete removes the object.
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
