package instance

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeUsedPorts map[int]bool

func (f fakeUsedPorts) UsedBasePorts(_ context.Context) (map[int]bool, error) { return f, nil }

// `↯` The base is deliberately **not** Valheim's 2456. Allocate also probes the host (A6),
// so this test — which is about the *database* skip — used to fail on any machine actually
// running a server in the default range: it allocated 2466 because 2456 was taken in the
// fake and 2461 was bound by a real game. Found 3 Sep 2026, on the first dev box with a real
// instance running. A base nothing binds keeps the assertion about the one thing it means to
// assert; the host probe has its own test below, which binds the port itself.
func TestAllocateSkipsPortsAlreadyInTheDatabase(t *testing.T) {
	const base = 12456
	a := NewAllocator(fakeUsedPorts{base: true}, base, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != base+5 {
		t.Errorf("allocated %d, want %d (%d is taken)", got, base+5, base)
	}
}

// TestAllocateSkipsAHostBoundPort is 05 M1's own acceptance test: a process holding
// [::]:2461 only must not be handed 2461, proving the check covers both address families
// (A6) — a v4-only probe would report it free.
func TestAllocateSkipsAHostBoundPort(t *testing.T) {
	held, err := net.ListenUDP("udp6", &net.UDPAddr{Port: 2461})
	if err != nil {
		t.Skipf("could not bind udp6 in this environment: %v", err)
	}
	defer func() { _ = held.Close() }()

	a := NewAllocator(fakeUsedPorts{}, 2456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got == 2461 {
		t.Error("allocated the port held on udp6 — the check missed the v6 side")
	}
}

// TestAllocateSkipsAHostBoundQueryPort proves the +1 half of the pair is checked too.
func TestAllocateSkipsAHostBoundQueryPort(t *testing.T) {
	held, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 2462})
	if err != nil {
		t.Skipf("could not bind udp4 in this environment: %v", err)
	}
	defer func() { _ = held.Close() }()

	a := NewAllocator(fakeUsedPorts{}, 2456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got == 2461 {
		t.Error("allocated 2461 even though its query port 2462 is held")
	}
}

func TestAllocateExhausted(t *testing.T) {
	used := fakeUsedPorts{}
	for i := range maxPortScan {
		used[2456+i*5] = true
	}
	a := NewAllocator(used, 2456, 5)
	if _, err := a.Allocate(t.Context()); !errors.Is(err, ErrPortsExhausted) {
		t.Errorf("err = %v, want ErrPortsExhausted", err)
	}
}
