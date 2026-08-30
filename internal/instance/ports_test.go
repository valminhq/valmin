package instance

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeUsedPorts map[int]bool

func (f fakeUsedPorts) UsedBasePorts(_ context.Context) (map[int]bool, error) { return f, nil }

func TestAllocateSkipsPortsAlreadyInTheDatabase(t *testing.T) {
	a := NewAllocator(fakeUsedPorts{2456: true}, 2456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != 2461 {
		t.Errorf("allocated %d, want 2461 (2456 is taken)", got)
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
