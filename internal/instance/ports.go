package instance

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// maxPortScan bounds the search. This is a friend-group panel, not a hosting business
// (01 §4 N3) — a few hundred candidate base ports is generous headroom, not a limit anyone
// should ever hit.
const maxPortScan = 1000

// ErrPortsExhausted reports that no free base port was found within maxPortScan candidates.
var ErrPortsExhausted = errors.New("no free port range left on this host")

// UsedPorts is what the allocator needs from the database: every base_port already
// reserved by a durable instance row.
type UsedPorts interface {
	UsedBasePorts(ctx context.Context) (map[int]bool, error)
}

// Allocator finds a free base port pair, stride 5 from ports.base (03 §2).
type Allocator struct {
	db           UsedPorts
	base, stride int
}

func NewAllocator(db UsedPorts, base, stride int) *Allocator {
	return &Allocator{db: db, base: base, stride: stride}
}

// Allocate returns the next free base port: not already reserved by another instance row, and
// not held on the host under either address family for either half of the pair (03 §2). Docker
// publishes on `0.0.0.0` and `[::]`, so the host check covers both `udp4` and `udp6` (A6).
func (a *Allocator) Allocate(ctx context.Context) (int, error) {
	used, err := a.db.UsedBasePorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("list used base ports: %w", err)
	}

	for i := range maxPortScan {
		port := a.base + i*a.stride
		if used[port] {
			continue
		}
		if hostFree(port) && hostFree(port+1) {
			return port, nil
		}
	}
	return 0, ErrPortsExhausted
}

// hostFree reports whether port is free on the host, in both address families, for UDP, the
// only protocol Valheim uses (03 §2). A point-in-time check, not a reservation; the caller's
// INSERT carries base_port UNIQUE as the race backstop.
func hostFree(port int) bool {
	for _, network := range []string{"udp4", "udp6"} {
		addr := &net.UDPAddr{Port: port}
		conn, err := net.ListenUDP(network, addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
	}
	return true
}
