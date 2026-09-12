package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// The destination policy of 05 M6. A user-supplied URL the panel will POST to is an SSRF
// primitive: from the panel's network position it reaches localhost, the host's LAN and any
// cloud metadata endpoint. Admin-only is necessary and not sufficient, so the address itself
// is constrained (ADR-167, closing Q33).
const (
	// dialTimeout and requestTimeout bound one attempt. Three of them plus backoff stay
	// inside deliveryDeadline.
	dialTimeout    = 5 * time.Second
	requestTimeout = 15 * time.Second

	// MaxAttempts and DeliveryDeadline are the retry policy of 12: at most three attempts
	// inside two minutes, and then the delivery is exhausted rather than retried forever.
	// It applies to webhook delivery alone and widens no world or container retry.
	MaxAttempts      = 3
	DeliveryDeadline = 2 * time.Minute

	// retryBackoff is the wait before attempts 2 and 3.
	retryBackoff = 5 * time.Second

	// maxResponse bounds what a receiver can make the panel read. The body is never shown,
	// only drained, so the connection can be reused and a hostile receiver cannot stream.
	maxResponse = 8 << 10
)

// ErrRejectedAddress reports a destination the policy refuses. It is a configuration fault,
// not a transport one: the caller answers 422 rather than retrying.
var ErrRejectedAddress = errors.New("destination address is not allowed")

// blockedPrefixes are the ranges netip's own predicates do not name: carrier-grade NAT, the
// IETF protocol assignments block that holds the cloud metadata discovery address,
// benchmarking, and the reserved top of the space.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
}

// Target is a destination that passed the policy: the URL as written, and the one address the
// panel will connect to. Pinning the address is what closes the rebinding window — the name is
// resolved once and the dial goes to that answer, while TLS still verifies the hostname.
type Target struct {
	URL  *url.URL
	Addr netip.Addr
}

// Parse applies every check that needs no network. HTTPS on port 443 only, no embedded
// credentials, and an IP literal is held to the same address policy a resolved name is.
func Parse(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRejectedAddress, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%w: the scheme must be https", ErrRejectedAddress)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: the URL must not embed credentials", ErrRejectedAddress)
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, fmt.Errorf("%w: the port must be 443", ErrRejectedAddress)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: the URL has no host", ErrRejectedAddress)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := allowed(addr); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// Sender posts rendered payloads. The zero value is the shipped policy: every field is a
// seam a test replaces, and nil means the real thing.
type Sender struct {
	// Lookup resolves a destination host. Nil is the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Client builds the client for one validated target. Nil is the pinned-address client
	// below, which is what keeps the connection on the address the policy approved.
	Client func(Target) *http.Client
	// Backoff is the wait between attempts. Zero is retryBackoff.
	Backoff time.Duration
}

func (s *Sender) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if s.Lookup != nil {
		return s.Lookup(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", host, err)
	}
	return addrs, nil
}

// client is where the redirect refusal is fixed, on whatever client the seam returns: a
// receiver that answers 302 is asking the panel to make a request the policy never checked,
// and that must not depend on which client built the connection.
func (s *Sender) client(target Target) *http.Client {
	c := pinnedClient(target)
	if s.Client != nil {
		c = s.Client(target)
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errNoRedirect }
	return c
}

func (s *Sender) backoff() time.Duration {
	if s.Backoff > 0 {
		return s.Backoff
	}
	return retryBackoff
}

// Resolve parses raw and picks the address the panel will connect to. Every answer is checked,
// not only the one chosen: a name that resolves to a public address and a private one is a
// rebinding attempt, not a multihomed host worth guessing about.
func (s *Sender) Resolve(ctx context.Context, raw string) (Target, error) {
	u, err := Parse(raw)
	if err != nil {
		return Target{}, err
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil {
		return Target{URL: u, Addr: addr.Unmap()}, nil
	}
	addrs, err := s.lookup(ctx, host)
	if err != nil {
		return Target{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return Target{}, fmt.Errorf("%w: %s resolves to nothing", ErrRejectedAddress, host)
	}
	for _, addr := range addrs {
		if err := allowed(addr.Unmap()); err != nil {
			return Target{}, err
		}
	}
	return Target{URL: u, Addr: addrs[0].Unmap()}, nil
}

// allowed reports whether one address is a public destination. IPv4-mapped IPv6 is unmapped
// first, so ::ffff:127.0.0.1 is the loopback it is rather than a global unicast address.
func allowed(addr netip.Addr) error {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return fmt.Errorf("%w: not an address", ErrRejectedAddress)
	case addr.IsUnspecified(), addr.IsLoopback(), addr.IsPrivate(),
		addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return fmt.Errorf("%w: %s is not a public address", ErrRejectedAddress, addr)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s is not a public address", ErrRejectedAddress, addr)
		}
	}
	return nil
}

// errNoRedirect refuses a redirect rather than following one. A receiver that answers 302 is
// asking the panel to make a request the policy never checked.
var errNoRedirect = errors.New("the destination answered with a redirect")

// Send delivers body to raw, retrying transport failures, 429 and 5xx up to MaxAttempts
// inside DeliveryDeadline. It returns how many attempts were made, so an exhausted delivery
// records what it spent.
//
// No error it returns carries the URL. The URL is a bearer credential, and net/url's own
// errors quote the whole thing — including the path, which for a Discord destination is the
// entire secret (11 §9).
func (s *Sender) Send(ctx context.Context, raw string, body Body) (attempts int, err error) {
	ctx, cancel := context.WithTimeout(ctx, DeliveryDeadline)
	defer cancel()

	for attempts = 1; ; attempts++ {
		err = s.attempt(ctx, raw, body)
		if err == nil {
			return attempts, nil
		}
		if attempts >= MaxAttempts || !retryable(err) {
			return attempts, err
		}
		select {
		case <-ctx.Done():
			return attempts, fmt.Errorf("%w after %d attempts", err, attempts)
		case <-time.After(s.backoff()):
		}
	}
}

// retryableError marks a failure worth another attempt: a transport fault, a 429, or a 5xx.
// Everything else — a rejected address, a 4xx, a redirect — is the destination's settled
// answer and is not retried.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func retryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

func (s *Sender) attempt(ctx context.Context, raw string, body Body) error {
	target, err := s.Resolve(ctx, raw)
	if err != nil {
		if errors.Is(err, ErrRejectedAddress) {
			return err
		}
		return &retryableError{err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target.URL.String(), bytes.NewReader(body.Bytes))
	if err != nil {
		return fmt.Errorf("build request: %w", sanitize(err))
	}
	req.Header.Set("Content-Type", body.ContentType)
	req.Header.Set("User-Agent", "valmin")

	resp, err := s.client(target).Do(req)
	if err != nil {
		if errors.Is(err, errNoRedirect) {
			return errNoRedirect
		}
		return &retryableError{err: fmt.Errorf("post to %s: %w", target.URL.Hostname(), sanitize(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))

	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return &retryableError{err: fmt.Errorf("%s answered %d", target.URL.Hostname(), resp.StatusCode)}
	default:
		return fmt.Errorf("%s answered %d", target.URL.Hostname(), resp.StatusCode)
	}
}

// pinnedClient dials the address the policy validated while leaving the request's hostname
// intact, so TLS verification is still against the name the operator configured.
func pinnedClient(target Target) *http.Client {
	pinned := netip.AddrPortFrom(target.Addr, 443).String()
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: dialTimeout}
				return d.DialContext(ctx, network, pinned)
			},
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: dialTimeout,
		},
	}
}

// sanitize strips the URL out of a transport error. *url.Error quotes the request URL in its
// message, and the panel's own logs and job records are places that URL must never reach.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
