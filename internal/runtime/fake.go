package runtime

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// Fake is an in-memory Runtime. It exists so that every package above this one is
// testable without a Docker daemon.
type Fake struct {
	// OnStart runs at Start, with the container to script. It is where a test makes a
	// container exit, OOM-kill or produce log output. It runs under the Fake's lock, so
	// it must not call back into the Fake.
	OnStart func(*FakeContainer)

	mu   sync.Mutex
	byID map[string]*FakeContainer
	next int
}

// FakeContainer is one scripted container.
type FakeContainer struct {
	Container
	Spec  ContainerSpec
	Stats Stats

	log  bytes.Buffer
	done chan struct{}
}

// Exit stops the container with the given code.
func (c *FakeContainer) Exit(code int) {
	if !c.Running {
		return
	}
	c.Running = false
	c.ExitCode = code
	c.FinishedAt = time.Now()
	close(c.done)
}

// OOMKill exits the container the way the kernel does, which is a SIGKILL (03 §3.3).
func (c *FakeContainer) OOMKill() {
	c.OOMKilled = true
	c.Exit(137)
}

// Stdout appends to the container's log. The bytes carry Docker's multiplex header, so a
// reader exercises the same demux path it uses against a daemon (E5).
func (c *FakeContainer) Stdout(s string) { c.write(stdcopy.Stdout, s) }

// Stderr appends to the container's log on the stderr stream.
func (c *FakeContainer) Stderr(s string) { c.write(stdcopy.Stderr, s) }

func (c *FakeContainer) write(stream stdcopy.StdType, s string) {
	_, _ = io.WriteString(stdcopy.NewStdWriter(&c.log, stream), s)
}

func NewFake() *Fake {
	return &Fake{byID: map[string]*FakeContainer{}}
}

// Get returns a container for a test to inspect or script. It returns nil if there is no
// such container.
func (f *Fake) Get(id string) *FakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byID[id]
}

func (f *Fake) Create(_ context.Context, spec *ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.next++
	id := "fake" + strconv.Itoa(f.next)
	f.byID[id] = &FakeContainer{
		Container: Container{ID: id, Name: spec.Name, Image: spec.Image, Labels: maps.Clone(spec.Labels)},
		Spec:      *spec,
		done:      make(chan struct{}),
	}
	return id, nil
}

func (f *Fake) Start(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return err
	}
	c.Running = true
	c.StartedAt = time.Now()
	if f.OnStart != nil {
		f.OnStart(c)
	}
	return nil
}

func (f *Fake) Stop(ctx context.Context, id, _ string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return err
	}
	c.Exit(0)
	return nil
}

func (f *Fake) Wait(ctx context.Context, id string) (int, error) {
	f.mu.Lock()
	c, err := f.get(ctx, id)
	if err != nil {
		f.mu.Unlock()
		return 0, err
	}
	done := c.done
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return 0, fmt.Errorf("wait for container %s: %w", id, ctx.Err())
	case <-done:
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return c.ExitCode, nil
}

func (f *Fake) Logs(ctx context.Context, id string, _ LogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(c.log.Bytes()))), nil
}

func (f *Fake) Stats(ctx context.Context, id string) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return Stats{}, err
	}
	return c.Stats, nil
}

func (f *Fake) Inspect(ctx context.Context, id string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return Container{}, err
	}
	return c.Container, nil
}

func (f *Fake) List(_ context.Context, labels map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Container, 0, len(f.byID))
	for _, c := range f.byID {
		match := true
		for k, v := range labels {
			if c.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, c.Container)
		}
	}
	slices.SortFunc(out, func(a, b Container) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (f *Fake) Remove(ctx context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := f.get(ctx, id)
	if err != nil {
		return err
	}
	if c.Running && !force {
		return fmt.Errorf("remove container %s: still running", id)
	}
	c.Exit(0)
	delete(f.byID, id)
	return nil
}

// get resolves a container, refusing work on a cancelled context the way the daemon
// does. Without that the fake would hide every context-propagation bug above it.
func (f *Fake) get(ctx context.Context, id string) (*FakeContainer, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("container %s: %w", id, err)
	}
	c, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("container %s: %w", id, ErrNotFound)
	}
	return c, nil
}
