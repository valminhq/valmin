package main

import "testing"

// TestProbeAddrDialsLoopbackForAWildcardBind asserts the health check connects to an address
// that exists: a wildcard is what the daemon binds, not somewhere to send a request.
func TestProbeAddrDialsLoopbackForAWildcardBind(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{"[::]:8080", "127.0.0.1:8080"},
		{"127.0.0.1:9000", "127.0.0.1:9000"},
		{"[::1]:9000", "[::1]:9000"},
	} {
		if got := probeAddr(tc.listen); got != tc.want {
			t.Errorf("probeAddr(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}
