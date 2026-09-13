package instance

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/valminhq/valmin/internal/runtime"
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

// ContainerLister is what the allocator needs from the engine: every container on this host,
// the panel's own and anyone else's, so their published ports can be read off.
type ContainerLister interface {
	List(ctx context.Context, labels map[string]string) ([]runtime.Container, error)
}

// Allocator finds a free base port pair, stride 5 from ports.base (03 §2).
type Allocator struct {
	db           UsedPorts
	rt           ContainerLister
	base, stride int
}

func NewAllocator(db UsedPorts, rt ContainerLister, base, stride int) *Allocator {
	return &Allocator{db: db, rt: rt, base: base, stride: stride}
}

// Allocate returns the next free base port: not reserved by another instance row, not published
// by any container on this host, and not held in this process's own network namespace (03 §2).
func (a *Allocator) Allocate(ctx context.Context) (int, error) {
	used, err := a.db.UsedBasePorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("list used base ports: %w", err)
	}
	published, err := a.publishedPorts(ctx)
	if err != nil {
		return 0, err
	}

	for i := range maxPortScan {
		port := a.base + i*a.stride
		if used[port] || published[port] || published[port+1] {
			continue
		}
		if localFree(port) && localFree(port+1) {
			return port, nil
		}
	}
	return 0, ErrPortsExhausted
}

// publishedPorts is every UDP host port a container on this host publishes. It is the host-level
// conflict check of 03 §2 in the only form available to a panel that is itself a container.
//
// Stopped containers count. A sibling configured on a port takes it back the moment it starts,
// and a base port is reserved for an instance's lifetime rather than for one run.
//
// A nil engine reports nothing rather than failing: the allocator is constructed on paths that
// predate the runtime, and a panel that cannot ask is no worse off than one that never asked.
func (a *Allocator) publishedPorts(ctx context.Context) (map[int]bool, error) {
	if a.rt == nil {
		return nil, nil
	}
	containers, err := a.rt.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list containers for port conflicts: %w", err)
	}
	taken := map[int]bool{}
	for i := range containers {
		for _, p := range containers[i].Spec.Ports {
			// UDP only. Valheim has no TCP listener (03 §2), so refusing a base because
			// something unrelated publishes that number on TCP walks the range for nothing.
			if p.Proto == "udp" && p.HostPort != 0 {
				taken[p.HostPort] = true
			}
		}
	}
	return taken, nil
}

// localFree reports whether port is free in this process's own network namespace, in both
// address families, for UDP, the only protocol Valheim uses (03 §2).
//
// `↯` That is the host's namespace only when the panel runs on the host. Under the shipped
// Compose the panel is on a bridge network (02 §5), where this binds inside the container and
// can see no host listener at all — so it is the weaker of the two checks here, not the primary
// one. What it adds, on a host-run panel, is the listener no container published: a server
// someone started by hand. A6's both-address-families requirement lives here because that is
// the case where it means anything.
//
// A point-in-time check, not a reservation; the caller's INSERT carries base_port UNIQUE as the
// race backstop.
func localFree(port int) bool {
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
