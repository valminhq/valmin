package middleware

import (
	"context"
	"net/netip"
	"testing"
)

// TestClientKeyGroupsAnIPv6ClientByItsSlash64 asserts that IPv6 addresses in one /64 share a
// key, and that IPv4, including IPv4-mapped IPv6, keys on the address.
func TestClientKeyGroupsAnIPv6ClientByItsSlash64(t *testing.T) {
	cases := []struct{ addr, want string }{
		{"203.0.113.7", "203.0.113.7"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff::9", "2001:db8:1:2::/64"},
	}
	for _, tc := range cases {
		ctx := context.WithValue(context.Background(), ctxClientIP, netip.MustParseAddr(tc.addr))
		if got := ClientKey(ctx); got != tc.want {
			t.Errorf("ClientKey(%s) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}
