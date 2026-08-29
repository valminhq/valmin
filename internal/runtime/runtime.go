package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// Runtime is the container engine behind the narrow interface of 02 §2.5. It holds no
// game-specific knowledge: everything Valheim about a container arrives in ContainerSpec.
type Runtime interface {
	Create(ctx context.Context, spec *ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id, signal string, timeout time.Duration) error
	Wait(ctx context.Context, id string) (int, error)
	Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error)
	Stats(ctx context.Context, id string) (Stats, error)
	Inspect(ctx context.Context, id string) (Container, error)
	List(ctx context.Context, labels map[string]string) ([]Container, error)
	Remove(ctx context.Context, id string, force bool) error
}

// ErrNotFound reports that no container with the given id exists. Callers distinguish it
// from a daemon failure without importing the Docker SDK: reconciliation treats a missing
// container as a fact about the world, not as an error (08 §6.1).
var ErrNotFound = errors.New("container not found")

// ContainerSpec is everything the panel sets when creating a container. It carries no
// defaults of its own; the fields the engine must never vary are applied by the
// implementation and are deliberately absent here:
//
//   - the capability set is empty and no-new-privileges is always on (ADR-026, 08 §2.2)
//   - swap is always disabled, MemorySwap tracking MemoryBytes (08 §5)
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	// User is "uid:gid". Empty runs as the image's user.
	User string

	// OpenStdin, StdinOnce and TTY cannot be changed after creation (A1, 07 §3).
	OpenStdin bool
	StdinOnce bool
	TTY       bool

	StopSignal  string
	StopTimeout time.Duration

	Binds []Bind
	Ports []Port

	// NetworkDisabled attaches the container to no network at all, which is what the
	// host_data_root self-check requires of its throwaway (10 §1.2).
	NetworkDisabled bool

	// RestartPolicy is the Docker policy name. Empty means no restart policy.
	RestartPolicy string

	MemoryBytes int64
	NanoCPUs    int64
}

// Bind is one host path mounted into the container. HostPath is always a path as the
// host sees it, never as the panel container sees it (10 §1.2).
type Bind struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// Port publishes one container port on the host.
type Port struct {
	HostPort      int
	ContainerPort int
	// Proto is "tcp" or "udp".
	Proto string
}

// LogOptions selects which part of a container's log stream to read.
type LogOptions struct {
	Follow bool
	// Timestamps prefixes each line with the engine's receive time. The log reader uses
	// them rather than its own clock, so a reader restart cannot move timestamps
	// backwards (14 §4.1).
	Timestamps bool
	// Tail is the number of lines to read from the end. Zero reads the whole log.
	Tail int
}

// Container is the observable state of one container.
type Container struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string

	Running      bool
	ExitCode     int
	OOMKilled    bool
	RestartCount int
	StartedAt    time.Time
	FinishedAt   time.Time
}

// Stats is one raw sample. The counters are cumulative and no percentage is derived
// here: CPU is a delta between two samples and belongs to the caller that holds both
// (E10, 14 §4.3).
type Stats struct {
	CPUNanos    uint64
	SystemNanos uint64
	OnlineCPUs  uint32
	// MemBytes is memory.current less inactive_file, which is what docker stats reports
	// (E11, Q24).
	MemBytes uint64
	MemLimit uint64
}
