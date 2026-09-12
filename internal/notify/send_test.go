package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// public is an address outside every blocked range, standing in for a real receiver.
var public = netip.MustParseAddr("203.0.113.5")

// TestParseRefusesEveryAddressTheDestinationPolicyExcludes asserts that the checks needing no
// network all happen before a request is built: scheme, port, embedded credentials, and an IP
// literal that names something on the panel's own side of the network.
func TestParseRefusesEveryAddressTheDestinationPolicyExcludes(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"plain http", "http://example.com/hook"},
		{"a non-https scheme", "gopher://example.com/hook"},
		{"embedded credentials", "https://user:secret@example.com/hook"},
		{"another port", "https://example.com:8443/hook"},
		{"loopback", "https://127.0.0.1/hook"},
		{"an ipv4-mapped loopback", "https://[::ffff:127.0.0.1]/hook"},
		{"ipv6 loopback", "https://[::1]/hook"},
		{"a private range", "https://10.1.2.3/hook"},
		{"link-local metadata", "https://169.254.169.254/hook"},
		{"the protocol assignments block", "https://192.0.0.192/hook"},
		{"carrier-grade nat", "https://100.64.0.1/hook"},
		{"a unique local address", "https://[fd00::1]/hook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.url); !errors.Is(err, ErrRejectedAddress) {
				t.Errorf("Parse(%s) = %v, want ErrRejectedAddress", tc.name, err)
			}
		})
	}

	if _, err := Parse("https://discord.com/api/webhooks/1/token"); err != nil {
		t.Errorf("Parse of a public https destination: %v", err)
	}
	if _, err := Parse("https://discord.com:443/api/webhooks/1/token"); err != nil {
		t.Errorf("Parse with an explicit 443: %v", err)
	}
}

// TestResolveRejectsANameThatAnswersWithAPrivateAddress asserts that every DNS answer is held
// to the policy, not only the one that would be dialled: a name answering with both a public
// and a private address is a rebinding attempt.
func TestResolveRejectsANameThatAnswersWithAPrivateAddress(t *testing.T) {
	s := &Sender{Lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{public, netip.MustParseAddr("127.0.0.1")}, nil
	}}
	if _, err := s.Resolve(t.Context(), "https://example.com/hook"); !errors.Is(err, ErrRejectedAddress) {
		t.Fatalf("Resolve = %v, want ErrRejectedAddress", err)
	}

	s.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{public}, nil
	}
	target, err := s.Resolve(t.Context(), "https://example.com/hook")
	if err != nil {
		t.Fatalf("Resolve of a public name: %v", err)
	}
	if target.Addr != public {
		t.Errorf("pinned address = %s, want %s", target.Addr, public)
	}
}

// receiver stands a TLS server in for the destination and reports how many requests reached
// it. The certificate names example.com, which is what the sender verifies: the address is
// pinned, the hostname is not rewritten.
func receiver(t *testing.T, handler http.HandlerFunc) (*Sender, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the test server's client is not backed by an *http.Transport")
	}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return &Sender{
		Lookup:  func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{public}, nil },
		Client:  func(Target) *http.Client { return client },
		Backoff: time.Millisecond,
	}, &hits
}

func body() Body { return Body{ContentType: "application/json", Bytes: []byte(`{"a":1}`)} }

// TestSendStopsAtTheFirstAcceptance asserts the ordinary path: one attempt, one request.
func TestSendStopsAtTheFirstAcceptance(t *testing.T) {
	s, hits := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	attempts, err := s.Send(t.Context(), "https://example.com/hook", body())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if attempts != 1 || hits.Load() != 1 {
		t.Errorf("attempts = %d, requests = %d, want 1 and 1", attempts, hits.Load())
	}
}

// TestSendRetriesAServerFaultAndThenGivesUp asserts the retry policy of 12: transport
// failures, 429 and 5xx are retried to MaxAttempts and then the delivery is exhausted rather
// than retried forever.
func TestSendRetriesAServerFaultAndThenGivesUp(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		s, hits := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		attempts, err := s.Send(t.Context(), "https://example.com/hook", body())
		if err == nil {
			t.Fatalf("Send to a receiver answering %d succeeded", status)
		}
		if attempts != MaxAttempts || int(hits.Load()) != MaxAttempts {
			t.Errorf("status %d: attempts = %d, requests = %d, want %d each",
				status, attempts, hits.Load(), MaxAttempts)
		}
	}
}

// TestSendDoesNotRetryTheDestinationsOwnAnswer asserts that a 4xx is settled: a wrong or
// revoked webhook URL is not a fault that goes away on the next attempt.
func TestSendDoesNotRetryTheDestinationsOwnAnswer(t *testing.T) {
	s, hits := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	attempts, err := s.Send(t.Context(), "https://example.com/hook", body())
	if err == nil {
		t.Fatal("Send to a receiver answering 401 succeeded")
	}
	if attempts != 1 || hits.Load() != 1 {
		t.Errorf("attempts = %d, requests = %d, want 1 and 1", attempts, hits.Load())
	}
}

// TestSendRefusesARedirect asserts that a 3xx is not followed: the address the policy checked
// is the only one the panel connects to.
func TestSendRefusesARedirect(t *testing.T) {
	s, _ := receiver(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	if _, err := s.Send(t.Context(), "https://example.com/hook", body()); !errors.Is(err, errNoRedirect) {
		t.Fatalf("Send = %v, want the redirect refusal", err)
	}
}

// TestSendErrorsNeverQuoteTheDestinationURL asserts 11 §9 on the one path that would otherwise
// leak it: net/url quotes the whole request URL in its errors, and for a Discord destination
// the path is the entire credential.
func TestSendErrorsNeverQuoteTheDestinationURL(t *testing.T) {
	const secret = "s3cr3t-token-path"
	s, _ := receiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := s.Send(t.Context(), "https://example.com/api/webhooks/1/"+secret, body())
	if err == nil {
		t.Fatal("Send to a receiver answering 401 succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error quotes the destination URL: %v", err)
	}

	// The same on a transport failure, which is where *url.Error carries it.
	dead := &Sender{
		Lookup:  func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{public}, nil },
		Backoff: time.Millisecond,
		Client: func(Target) *http.Client {
			return &http.Client{Transport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("connection refused")
				},
			}}
		},
	}
	_, err = dead.Send(t.Context(), "https://example.com/api/webhooks/1/"+secret, body())
	if err == nil {
		t.Fatal("Send through a dead dialler succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("transport error quotes the destination URL: %v", err)
	}
}
