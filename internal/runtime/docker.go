package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Docker implements Runtime against a Docker daemon.
type Docker struct {
	cli client.APIClient
}

// dropAllCaps and noNewPrivileges are applied to every container the panel creates.
// They are not in ContainerSpec because an empty capability set is a trust-boundary
// property rather than a per-container setting (ADR-026, 08 §2.2).
var (
	dropAllCaps     = []string{"ALL"}
	noNewPrivileges = []string{"no-new-privileges"}
)

// NewDocker connects to the daemon and verifies it answers. An empty apiVersion
// negotiates against the daemon; pin one only if negotiation misbehaves.
//
// The ping is the startup gate's "Docker daemon reachable" check (10 §2): negotiation is
// otherwise lazy, and the first failure would surface inside a job instead of at boot.
func NewDocker(ctx context.Context, endpoint, apiVersion string) (*Docker, error) {
	opts := []client.Opt{client.WithHost(endpoint)}
	if apiVersion == "" {
		opts = append(opts, client.WithAPIVersionNegotiation())
	} else {
		opts = append(opts, client.WithVersion(apiVersion))
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client for %s: %w", endpoint, err)
	}

	ping, err := cli.Ping(ctx)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("docker daemon unreachable at %s: %w", endpoint, err)
	}
	slog.InfoContext(ctx, "connected to docker",
		slog.String("endpoint", endpoint), slog.String("api_version", ping.APIVersion))

	return &Docker{cli: cli}, nil
}

// Close releases the daemon connection.
func (d *Docker) Close() error {
	if err := d.cli.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}
	return nil
}

func (d *Docker) Create(ctx context.Context, spec *ContainerSpec) (string, error) {
	exposed, bindings := portMaps(spec.Ports)

	cfg := &container.Config{
		Image:        spec.Image,
		Entrypoint:   spec.Entrypoint,
		Cmd:          spec.Cmd,
		Env:          spec.Env,
		Labels:       spec.Labels,
		User:         spec.User,
		OpenStdin:    spec.OpenStdin,
		StdinOnce:    spec.StdinOnce,
		Tty:          spec.TTY,
		StopSignal:   spec.StopSignal,
		ExposedPorts: exposed,
	}
	if spec.StopTimeout > 0 {
		secs := int(spec.StopTimeout.Seconds())
		cfg.StopTimeout = &secs
	}

	host := &container.HostConfig{
		Binds:        binds(spec.Binds),
		PortBindings: bindings,
		CapDrop:      dropAllCaps,
		SecurityOpt:  noNewPrivileges,
		Resources: container.Resources{
			Memory: spec.MemoryBytes,
			// Equal to Memory disables swap. Left unset, Docker grants twice the limit,
			// and swapping a Unity simulation is worse than useless (08 §5).
			MemorySwap: spec.MemoryBytes,
			NanoCPUs:   spec.NanoCPUs,
		},
	}
	if spec.RestartPolicy != "" {
		host.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyMode(spec.RestartPolicy)}
	}
	if spec.NetworkDisabled {
		host.NetworkMode = "none"
	}

	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create container %s from %s: %w", spec.Name, spec.Image, err)
	}
	slog.InfoContext(ctx, "created container",
		slog.String("container_id", created.ID),
		slog.String("name", spec.Name),
		slog.String("image", spec.Image))
	return created.ID, nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	if err := d.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return wrap(err, "start container %s", id)
	}
	slog.InfoContext(ctx, "started container", slog.String("container_id", id))
	return nil
}

// Stop signals the container and waits for it to exit, escalating to SIGKILL only after
// timeout. The floor on that timeout is the caller's: below it Docker kills the process
// mid-write (ADR-008).
func (d *Docker) Stop(ctx context.Context, id, signal string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	opts := container.StopOptions{Signal: signal, Timeout: &secs}

	slog.InfoContext(ctx, "stopping container",
		slog.String("container_id", id),
		slog.String("signal", signal),
		slog.Int("timeout_seconds", secs))

	if err := d.cli.ContainerStop(ctx, id, opts); err != nil {
		return wrap(err, "stop container %s", id)
	}
	return nil
}

// Wait blocks until the container is no longer running and returns its exit code. A
// container that has already exited returns immediately.
func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	okC, errC := d.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errC:
		return 0, wrap(err, "wait for container %s", id)
	case ok := <-okC:
		if ok.Error != nil && ok.Error.Message != "" {
			return int(ok.StatusCode), fmt.Errorf("wait for container %s: %s", id, ok.Error.Message)
		}
		return int(ok.StatusCode), nil
	}
}

// Logs returns the container's log stream. With TTY false the stream carries Docker's
// 8-byte multiplex header, which the caller demuxes: a frame boundary can fall mid-line
// and one frame can carry several (E5, 14 §4.1).
func (d *Docker) Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error) {
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}

	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
		Tail:       tail,
	})
	if err != nil {
		return nil, wrap(err, "read logs of container %s", id)
	}
	return rc, nil
}

// Stats takes one sample. It does not open Docker's stats stream: the sampler ticks at
// its own interval and computes its own deltas, so a second cadence inside the adapter
// would only add a goroutine to reconcile with (14 §4.3).
func (d *Docker) Stats(ctx context.Context, id string) (Stats, error) {
	resp, err := d.cli.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return Stats{}, wrap(err, "read stats of container %s", id)
	}
	defer func() { _ = resp.Body.Close() }()

	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Stats{}, fmt.Errorf("decode stats of container %s: %w", id, err)
	}

	return Stats{
		CPUNanos:    raw.CPUStats.CPUUsage.TotalUsage,
		SystemNanos: raw.CPUStats.SystemUsage,
		OnlineCPUs:  raw.CPUStats.OnlineCPUs,
		MemBytes:    workingSet(raw.MemoryStats),
		MemLimit:    raw.MemoryStats.Limit,
	}, nil
}

// workingSet subtracts the page cache term, which is what docker stats reports. The
// cgroup v1 key differs from v2 (E11, Q24).
func workingSet(m container.MemoryStats) uint64 {
	cache, ok := m.Stats["inactive_file"]
	if !ok {
		cache = m.Stats["total_inactive_file"]
	}
	if cache > m.Usage {
		return 0
	}
	return m.Usage - cache
}

func (d *Docker) Inspect(ctx context.Context, id string) (Container, error) {
	resp, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return Container{}, wrap(err, "inspect container %s", id)
	}
	return toContainer(resp)
}

// List returns every container carrying all of the given labels, running or not. Each
// match is inspected, so the results carry the same fields as Inspect.
func (d *Docker) List(ctx context.Context, labels map[string]string) ([]Container, error) {
	args := filters.NewArgs()
	for k, v := range labels {
		args.Add("label", k+"="+v)
	}

	summaries, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, wrap(err, "list containers")
	}

	out := make([]Container, 0, len(summaries))
	for i := range summaries {
		c, err := d.Inspect(ctx, summaries[i].ID)
		if err != nil {
			// The container was removed between the list and the inspect. It is not
			// running now, which is all the caller was asking.
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (d *Docker) Remove(ctx context.Context, id string, force bool) error {
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
	if err != nil {
		return wrap(err, "remove container %s", id)
	}
	slog.InfoContext(ctx, "removed container", slog.String("container_id", id))
	return nil
}

func toContainer(resp container.InspectResponse) (Container, error) {
	started, err := parseTime(resp.State.StartedAt)
	if err != nil {
		return Container{}, fmt.Errorf("container %s StartedAt: %w", resp.ID, err)
	}
	finished, err := parseTime(resp.State.FinishedAt)
	if err != nil {
		return Container{}, fmt.Errorf("container %s FinishedAt: %w", resp.ID, err)
	}

	c := Container{
		ID:           resp.ID,
		Name:         strings.TrimPrefix(resp.Name, "/"),
		Running:      resp.State.Running,
		ExitCode:     resp.State.ExitCode,
		OOMKilled:    resp.State.OOMKilled,
		RestartCount: resp.RestartCount,
		StartedAt:    started,
		FinishedAt:   finished,
	}
	if resp.Config != nil {
		c.Image = resp.Config.Image
		c.Labels = resp.Config.Labels
	}
	return c, nil
}

// parseTime reads Docker's timestamps. The zero value "0001-01-01T00:00:00Z" means the
// event has not happened; a value that will not parse is reported rather than flattened
// to zero, because a silently zero StartedAt reads as a container that never ran.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %q: %w", s, err)
	}
	if t.IsZero() || t.Year() <= 1 {
		return time.Time{}, nil
	}
	return t, nil
}

func binds(bs []Bind) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		mode := "rw"
		if b.ReadOnly {
			mode = "ro"
		}
		out = append(out, b.HostPath+":"+b.ContainerPath+":"+mode)
	}
	return out
}

func portMaps(ps []Port) (nat.PortSet, nat.PortMap) {
	if len(ps) == 0 {
		return nil, nil
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range ps {
		key := nat.Port(strconv.Itoa(p.ContainerPort) + "/" + p.Proto)
		exposed[key] = struct{}{}
		bindings[key] = []nat.PortBinding{{HostPort: strconv.Itoa(p.HostPort)}}
	}
	return exposed, bindings
}

// wrap translates a daemon error, mapping "no such container" onto ErrNotFound so
// callers never import the Docker SDK to ask whether a container still exists.
func wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrNotFound)
	}
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
}
