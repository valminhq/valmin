// Package diag turns the panel's own startup and runtime facts into a report an operator
// can read, and a bundle they can attach to a bug report. It reads; it changes nothing.
//
// Specification: 06 §1 ADR-193.
package diag

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/version"
)

// Status is one check's verdict. Unknown is distinct from ok: a fact that was never
// measured must not render as a pass.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusUnknown Status = "unknown"
)

// Source is where a check's answer came from, so a reader can tell a live probe from a
// stamp recorded hours ago.
type Source string

const (
	// SourceLive was measured while serving this request.
	SourceLive Source = "live"
	// SourceStartup was recorded by the startup gate.
	SourceStartup Source = "startup"
	// SourceJob was recorded by a background job.
	SourceJob Source = "job"
	// SourceConfig is read from configuration and not probed.
	SourceConfig Source = "config"
)

// Check identifiers. They are stable: the SPA groups on them and a bundle is compared
// against an older one.
const (
	CheckDockerPing      = "docker.ping"
	CheckDockerAPI       = "docker.api_version"
	CheckGameImage       = "docker.game_image"
	CheckSteamCMDImage   = "docker.steamcmd_image"
	CheckFreeSpace       = "storage.free_space"
	CheckDataRootWrite   = "storage.data_root_writable"
	CheckFilesystem      = "storage.filesystem"
	CheckIdentity        = "storage.identity"
	CheckHostRoot        = "storage.host_data_root"
	CheckGameNetwork     = "network.game_network"
	CheckThunderstore    = "network.thunderstore"
	CheckSteam           = "network.steam"
	CheckExternalURL     = "https.external_url"
	CheckTrustedProxies  = "https.trusted_proxies"
	CheckPortPublication = "ports.publication"
)

// panelUID is the uid every panel and game process runs as (A3, 08 §2).
const panelUID = 10000

// Check is one diagnosed fact.
type Check struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Source Source `json:"source"`
	// Detail is what was observed, in prose this package composes.
	Detail string `json:"detail"`
	// Diagnostic is verbatim output from whatever was probed. It can name filesystem
	// paths and hostnames, so the support bundle omits it; the page shows it.
	Diagnostic string `json:"diagnostic,omitempty"`
	// Remedy is what to do about it, and is empty on a passing check.
	Remedy     string     `json:"remedy,omitempty"`
	MeasuredAt *time.Time `json:"measured_at,omitempty"`
}

// Observation is a check outcome recorded earlier, by the startup gate or a job.
type Observation struct {
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Instance is one server's diagnosable state. Nothing here is console content: those
// lines carry player identifiers (D14), so the report describes the startup segment
// rather than reproducing it.
type Instance struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Image           string `json:"image"`
	BasePort        int    `json:"base_port"`
	ExpectedPorts   []int  `json:"expected_ports"`
	BoundPorts      []int  `json:"bound_ports"`
	Mods            int    `json:"mods"`
	RestartRequired bool   `json:"restart_required"`
	Running         bool   `json:"running"`
	// LogReaderAttached reports whether a log reader is following this container. A
	// running instance without one has no console and no save-complete detection.
	LogReaderAttached bool `json:"log_reader_attached"`
	// ServerFreeBytes is the space the server reported from inside its own container, or
	// nil if it has not said. It can differ from the host's view of the same filesystem.
	ServerFreeBytes *uint64 `json:"server_free_bytes"`
	// PortIssue is why BoundPorts disagrees with ExpectedPorts, empty when they agree or
	// the instance is not running.
	PortIssue string `json:"port_issue,omitempty"`
}

// Report is one diagnostics run.
type Report struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Build       version.Build `json:"build"`
	StartedAt   time.Time     `json:"started_at"`
	Checks      []Check       `json:"checks"`
	Instances   []Instance    `json:"instances"`
	Migrations  []string      `json:"migrations"`
}

// Engine is the part of the container runtime a report probes.
type Engine interface {
	Ping(ctx context.Context) error
	APIVersion() string
	ImageExists(ctx context.Context, ref string) (bool, error)
}

// PackageIndex probes the mod index.
type PackageIndex interface {
	Reachable(ctx context.Context, etag string) error
}

// Input is everything Collect needs. The caller gathers the stored facts so that this
// package touches neither the database nor the filesystem.
type Input struct {
	Config  *config.Config
	Runtime Engine
	// Packages is the mod index prober. A nil prober leaves the Thunderstore check unknown.
	Packages PackageIndex

	Now       time.Time
	Build     version.Build
	StartedAt time.Time
	UID       int
	GID       int

	// FSType is the data root's filesystem, probed once at startup.
	FSType string
	// FreeBytes is what is available on the data root, and AlarmBytes the floor below
	// which the panel calls it low.
	FreeBytes  uint64
	AlarmBytes uint64

	Migrations []string
	// Recorded holds outcomes stamped by the startup gate or the diagnose job, keyed by
	// check id.
	Recorded map[string]Observation

	// ThunderstoreETag is sent with the reachability probe; ThunderstoreSyncedAt is when
	// the index last synced.
	ThunderstoreETag   string
	ThunderstoreSynced time.Time
	// SteamBuildID is the public build SteamCMD last reported, and SteamObservedAt when.
	SteamBuildID    string
	SteamObservedAt time.Time

	Instances []Instance
}

// Collect runs every check and returns the report. A failing check never aborts the run:
// it becomes a fail row carrying the error.
func Collect(ctx context.Context, in *Input) Report {
	r := Report{
		GeneratedAt: in.Now,
		Build:       in.Build,
		StartedAt:   in.StartedAt,
		Instances:   in.Instances,
		Migrations:  in.Migrations,
	}
	r.Checks = append(r.Checks,
		in.docker(ctx)...)
	r.Checks = append(r.Checks, in.storage()...)
	r.Checks = append(r.Checks, in.network(ctx)...)
	r.Checks = append(r.Checks, in.https()...)
	r.Checks = append(r.Checks, in.ports())
	return r
}

// docker probes the engine and the two images the panel runs but never pulls (ADR-048).
// A nil engine is the offline case: the command-line report runs without one.
func (in *Input) docker(ctx context.Context) []Check {
	if in.Runtime == nil {
		out := make([]Check, 0, 4)
		for _, id := range []struct{ id, title string }{
			{CheckDockerPing, "Engine reachable"},
			{CheckDockerAPI, "API version"},
			{CheckGameImage, "Game image present"},
			{CheckSteamCMDImage, "SteamCMD image present"},
		} {
			c := in.check(id.id, "Docker", id.title, SourceLive)
			c.unknown("No connection to the container engine.")
			out = append(out, c.Check)
		}
		return out
	}

	ping := in.live(CheckDockerPing, "Docker", "Engine reachable")
	if err := in.Runtime.Ping(ctx); err != nil {
		ping.fail("The engine did not answer.",
			"Check docker.endpoint and that the socket or proxy is running.")
		ping.verbatim(err.Error())
	} else {
		ping.ok("The engine answered.")
	}

	api := in.check(CheckDockerAPI, "Docker", "API version", SourceConfig)
	api.ok(in.Runtime.APIVersion())

	checks := []Check{ping.Check, api.Check}
	// The game image is also proven at every boot: the host_data_root self-check runs a
	// throwaway from it (10 §1.2). The SteamCMD image is not, so it is the one that first
	// fails inside a provision.
	for _, img := range []struct{ id, title, ref, remedy string }{
		{
			CheckGameImage, "Game image present", in.Config.Game.Image,
			"Pull or build game.image on the host; the panel never pulls it.",
		},
		{
			CheckSteamCMDImage, "SteamCMD image present", in.Config.Game.SteamCMDImage,
			"Pull game.steamcmd_image on the host; provisioning and update checks need it.",
		},
	} {
		c := in.live(img.id, "Docker", img.title)
		switch found, err := in.Runtime.ImageExists(ctx, img.ref); {
		case err != nil:
			c.fail("The engine would not answer for "+img.ref+".", img.remedy)
			c.verbatim(err.Error())
		case !found:
			c.fail(img.ref+" is not present locally.", img.remedy)
		default:
			c.ok(img.ref)
		}
		checks = append(checks, c.Check)
	}
	return checks
}

// storage reports the data root's headroom, filesystem and owning identity, and replays
// the host_data_root round-trip the startup gate already performed.
func (in *Input) storage() []Check {
	free := in.live(CheckFreeSpace, "Storage", "Free space")
	switch {
	case in.AlarmBytes > 0 && in.FreeBytes < in.AlarmBytes:
		free.fail(
			fmt.Sprintf("%d bytes free, below the %d byte floor.", in.FreeBytes, in.AlarmBytes),
			"Free space or move data.root. Valheim stops saving below ~6.4 MB without an error (B6).")
	default:
		free.ok(fmt.Sprintf("%d bytes free.", in.FreeBytes))
	}

	fs := in.check(CheckFilesystem, "Storage", "Filesystem", SourceStartup)
	if in.FSType == "" {
		fs.unknown("The data root's filesystem was not probed.")
	} else {
		fs.ok(in.FSType)
	}

	id := in.check(CheckIdentity, "Storage", "Process identity", SourceLive)
	if in.UID != panelUID || in.GID != panelUID {
		id.warn(fmt.Sprintf("Running as %d:%d, not %d:%d.", in.UID, in.GID, panelUID, panelUID),
			"Container uids are host uids on a bind mount (A3). Run the daemon as uid 10000.")
	} else {
		id.ok(fmt.Sprintf("Running as %d:%d.", in.UID, in.GID))
	}

	// The write probe creates a file, so it is a recorded outcome rather than something a
	// read-only GET repeats.
	writable := in.recorded(CheckDataRootWrite, "Storage", "Data root writable",
		"The panel cannot write to data.root as its own uid. Check the directory's owner "+
			"and mode, then re-run the deep checks.")
	hostRoot := in.recorded(CheckHostRoot, "Storage", "host_data_root round-trip",
		"Re-run the deep checks. data.host_root must be the path on the host, not the path "+
			"inside the panel container (10 §1.2).")

	return []Check{free.Check, writable.Check, fs.Check, id.Check, hostRoot.Check}
}

// network reports the two external services the panel depends on. Thunderstore is probed
// live; Steam is not, because only SteamCMD can prove it and that means a container.
func (in *Input) network(ctx context.Context) []Check {
	ts := in.live(CheckThunderstore, "Network", "Thunderstore reachable")
	switch err := in.reachIndex(ctx); {
	case in.Packages == nil:
		ts.unknown("No mod index client is configured.")
	case err != nil:
		ts.fail("The package listing did not answer.",
			"Check thunderstore.base_url and outbound HTTPS from the panel.")
		ts.verbatim(err.Error())
	default:
		ts.ok("The package listing answered. Last sync: " + stamp(in.ThunderstoreSynced))
	}

	steam := in.check(CheckSteam, "Network", "Steam reachable", SourceJob)
	if in.SteamBuildID == "" || in.SteamObservedAt.IsZero() {
		steam.unknown("SteamCMD has not reported a public build yet.")
		steam.Remedy = "Run the deep checks, or wait for the scheduled update check."
	} else {
		steam.at(in.SteamObservedAt)
		steam.ok("SteamCMD reported public build " + in.SteamBuildID + ".")
	}

	return []Check{
		in.recorded(CheckGameNetwork, "Network", "Game network reachable",
			"Attach the daemon to game.network, or every command to a running server times "+
				"out (ADR-190).").Check,
		ts.Check, steam.Check,
	}
}

// reachIndex probes the mod index, or reports nothing to probe.
func (in *Input) reachIndex(ctx context.Context) error {
	if in.Packages == nil {
		return nil
	}
	if err := in.Packages.Reachable(ctx, in.ThunderstoreETag); err != nil {
		return fmt.Errorf("reach the mod index: %w", err)
	}
	return nil
}

// https reports the two settings that decide whether a browser can hold a session.
func (in *Input) https() []Check {
	url := in.check(CheckExternalURL, "HTTPS", "External URL", SourceConfig)
	scheme, isLocal := externalURLShape(in.Config.Server.ExternalURL)
	switch {
	case config.CookiesUnstorable(in.Config.Server.ExternalURL):
		url.warn(
			"server.external_url is plain HTTP on a non-local host.",
			"Session and CSRF cookies are always Secure, so no browser will store them and "+
				"every request after login fails. Serve the panel over HTTPS.")
	case isLocal:
		url.ok("Served as " + scheme + " on a loopback host.")
	default:
		url.ok("Served as " + scheme + ".")
	}

	proxies := in.check(CheckTrustedProxies, "HTTPS", "Trusted proxies", SourceConfig)
	if n := len(in.Config.Server.TrustedProxies); n == 0 {
		proxies.ok("None. X-Forwarded-For is ignored and the socket peer address is used (D9).")
	} else {
		proxies.ok(fmt.Sprintf("%d CIDR range(s) trusted for X-Forwarded-For.", n))
	}

	return []Check{url.Check, proxies.Check}
}

// ports compares each running instance's allocated UDP ports against what the engine
// reports published. It does not test reachability from outside the host: that needs a
// third party, and a bind probe inside the panel container answers about the panel's own
// network namespace (03 §2).
func (in *Input) ports() Check {
	c := in.live(CheckPortPublication, "Ports", "UDP ports published")

	var problems []string
	for i := range in.Instances {
		inst := &in.Instances[i]
		if !inst.Running {
			continue
		}
		if !slices.Equal(sorted(inst.ExpectedPorts), sorted(inst.BoundPorts)) {
			inst.PortIssue = fmt.Sprintf("expected %v, engine publishes %v",
				sorted(inst.ExpectedPorts), sorted(inst.BoundPorts))
			problems = append(problems, inst.Name+": "+inst.PortIssue)
		}
	}
	if len(problems) > 0 {
		c.fail(fmt.Sprintf("%d running instance(s) disagree with the engine: %v",
			len(problems), problems),
			"Recreate the instance so its container is rebuilt from the current spec.")
		return c.Check
	}
	c.ok("Every running instance publishes the UDP ports it was allocated. " +
		"This does not test reachability from the internet.")
	return c.Check
}

// recorded renders an outcome the startup gate or the diagnose job stamped.
func (in *Input) recorded(id, group, title, remedy string) *builder {
	c := in.check(id, group, title, SourceStartup)
	obs, found := in.Recorded[id]
	if !found {
		c.unknown("Not recorded yet.")
		return c
	}
	c.at(obs.CheckedAt)
	if obs.OK {
		c.ok("Verified.")
		return c
	}
	c.fail("The last run of this check failed.", remedy)
	c.verbatim(obs.Detail)
	return c
}

// builder accumulates one Check so each probe reads as a verdict rather than a struct
// literal.
type builder struct{ Check }

func (in *Input) check(id, group, title string, src Source) *builder {
	return &builder{Check{
		ID: id, Group: group, Title: title, Source: src, Status: StatusUnknown,
		MeasuredAt: &in.Now,
	}}
}

func (in *Input) live(id, group, title string) *builder {
	return in.check(id, group, title, SourceLive)
}

func (b *builder) ok(detail string) { b.Status, b.Detail = StatusOK, detail }
func (b *builder) unknown(d string) { b.Status, b.Detail = StatusUnknown, d }
func (b *builder) at(t time.Time)   { b.MeasuredAt = &t }
func (b *builder) warn(d, r string) { b.Status, b.Detail, b.Remedy = StatusWarn, d, r }
func (b *builder) fail(d, r string) { b.Status, b.Detail, b.Remedy = StatusFail, d, r }

// verbatim attaches output from the probed thing, which this package did not compose and
// cannot vouch for.
func (b *builder) verbatim(s string) { b.Diagnostic = s }

// Worst is the most severe status in the report, for a page that leads with a verdict.
func (r *Report) Worst() Status {
	worst := StatusOK
	rank := map[Status]int{StatusOK: 0, StatusUnknown: 1, StatusWarn: 2, StatusFail: 3}
	for i := range r.Checks {
		if rank[r.Checks[i].Status] > rank[worst] {
			worst = r.Checks[i].Status
		}
	}
	return worst
}

func sorted(ports []int) []int {
	out := slices.Clone(ports)
	slices.Sort(out)
	return out
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
