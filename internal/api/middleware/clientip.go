package middleware

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP resolves the caller's address per 10 §5 and puts it in the request context. It
// sits above rate limiting and above anything that writes audit_log, or both record the
// reverse proxy instead of the caller.
//
// trusted is empty by default. Empty means the socket peer is used verbatim: a trusting
// default lets anyone spoof past the invite rate limiter with a header, and an audit trail
// that is visibly wrong beats one that is silently forgeable (D9).
func ClientIP(trusted []netip.Prefix) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr := resolveIP(r, trusted)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClientIP, addr)))
		})
	}
}

func resolveIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := peerAddr(r.RemoteAddr)
	if !isTrusted(peer, trusted) {
		// Headers from untrusted peers are ignored entirely, never merged.
		return peer
	}
	if fwd, ok := rightmostUntrusted(r.Header.Values("X-Forwarded-For"), trusted); ok {
		return fwd
	}
	return peer
}

// rightmostUntrusted walks X-Forwarded-For from the end and returns the first entry that
// is not itself a trusted proxy. Entries to the left of it were appended by hops the panel
// has no reason to believe.
func rightmostUntrusted(headers []string, trusted []netip.Prefix) (netip.Addr, bool) {
	for i := len(headers) - 1; i >= 0; i-- {
		entries := strings.Split(headers[i], ",")
		for j := len(entries) - 1; j >= 0; j-- {
			addr, err := netip.ParseAddr(strings.TrimSpace(entries[j]))
			if err != nil {
				continue
			}
			if addr = addr.Unmap(); !isTrusted(addr, trusted) {
				return addr, true
			}
		}
	}
	return netip.Addr{}, false
}

// peerAddr extracts the socket peer from RemoteAddr. A v4-mapped v6 address is unmapped so
// it matches a v4 CIDR, since Docker publishes on both families.
func peerAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap()
	}
	if addr, err := netip.ParseAddr(remote); err == nil {
		return addr.Unmap()
	}
	return netip.Addr{}
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
