package instance

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
)

// publishing returns a fake engine holding one container that publishes hostPort on UDP, which
// is what a sibling game server on this host looks like to the allocator.
func publishing(t *testing.T, hostPort int) *runtime.Fake {
	t.Helper()
	fake := runtime.NewFake()
	if _, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Image: "example.invalid/other", User: "10000:10000",
		Ports: []runtime.Port{
			{HostPort: hostPort, ContainerPort: hostPort, Proto: "udp"},
			{HostPort: hostPort + 1, ContainerPort: hostPort + 1, Proto: "udp"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return fake
}

type fakeUsedPorts map[int]bool

func (f fakeUsedPorts) UsedBasePorts(_ context.Context) (map[int]bool, error) { return f, nil }

// TestAllocateSkipsPortsAlreadyInTheDatabase asserts Allocate steps past a base the fake
// database marks used. The base is deliberately not Valheim's 2456: Allocate also probes
// the host (A6), and a base in the real default range would collide with any machine
// actually running a server there. This test is about the database skip alone; the host
// probe has its own test below, which binds the port itself.
func TestAllocateSkipsPortsAlreadyInTheDatabase(t *testing.T) {
	const base = 12456
	a := NewAllocator(fakeUsedPorts{base: true}, nil, base, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != base+5 {
		t.Errorf("allocated %d, want %d (%d is taken)", got, base+5, base)
	}
}

// TestAllocateSkipsAHostBoundPort asserts that a process holding only [::]:2461 is not
// handed 2461, proving the check covers both address families (A6) — a v4-only probe would
// report it free.
func TestAllocateSkipsAHostBoundPort(t *testing.T) {
	held, err := net.ListenUDP("udp6", &net.UDPAddr{Port: 2461})
	if err != nil {
		t.Skipf("could not bind udp6 in this environment: %v", err)
	}
	defer func() { _ = held.Close() }()

	a := NewAllocator(fakeUsedPorts{}, nil, 2456, 5)
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

	a := NewAllocator(fakeUsedPorts{}, nil, 2456, 5)
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
	a := NewAllocator(used, nil, 2456, 5)
	if _, err := a.Allocate(t.Context()); !errors.Is(err, ErrPortsExhausted) {
		t.Errorf("err = %v, want ErrPortsExhausted", err)
	}
}

// TestAllocateSkipsAPortPublishedByAnotherContainer is 03 §2's host-level check as it has to
// work once the panel is itself containerized. A bind probe then runs in the panel's own
// network namespace and can see no host listener at all, so the engine is what the panel asks.
// The container here is not the panel's: the panel's own are already in the database.
func TestAllocateSkipsAPortPublishedByAnotherContainer(t *testing.T) {
	// 12456 is taken in the database, so 12461 is the candidate the engine has to reject; the
	// answer must be the one after it.
	a := NewAllocator(fakeUsedPorts{12456: true}, publishing(t, 12461), 12456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != 12466 {
		t.Errorf("allocated %d, want 12466: another container already publishes 12461", got)
	}
}

// TestAllocateSkipsABaseWhoseQueryPortIsPublished proves the +1 half of the pair is checked
// against the engine too, not only the base.
func TestAllocateSkipsABaseWhoseQueryPortIsPublished(t *testing.T) {
	// 12462 is the query port of base 12461, and the only port this container publishes.
	fake := runtime.NewFake()
	if _, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Image: "example.invalid/other", User: "10000:10000",
		Ports: []runtime.Port{{HostPort: 12462, ContainerPort: 12462, Proto: "udp"}},
	}); err != nil {
		t.Fatal(err)
	}

	a := NewAllocator(fakeUsedPorts{12456: true}, fake, 12456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != 12466 {
		t.Errorf("allocated %d, want 12466: 12461's query port 12462 is published", got)
	}
}

// TestAllocateIgnoresTCPPublications guards the other direction: Valheim is UDP only (03 §2),
// and refusing a base because something unrelated publishes the same number on TCP would walk
// the range for no reason.
func TestAllocateIgnoresTCPPublications(t *testing.T) {
	fake := runtime.NewFake()
	if _, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Image: "example.invalid/web", User: "10000:10000",
		Ports: []runtime.Port{{HostPort: 12456, ContainerPort: 80, Proto: "tcp"}},
	}); err != nil {
		t.Fatal(err)
	}

	a := NewAllocator(fakeUsedPorts{}, fake, 12456, 5)
	got, err := a.Allocate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != 12456 {
		t.Errorf("allocated %d, want 12456: a TCP publication is not a Valheim port", got)
	}
}
