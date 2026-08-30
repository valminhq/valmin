package instance

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// containerUser is A3/08 §2: one fixed UID/GID for every panel-managed process and file.
const containerUser = "10000:10000"

// entrypoint is the fixed wrapper both the real image and the stub mirror (08 §4.2,
// docker/valheim-stub/Dockerfile). It sets the game's own environment and execs the
// binary with the argv BuildSpec produces — never a shell, so extra_args (D8) reaches
// the process as separate argv elements rather than a string a shell could reinterpret.
var entrypoint = []string{"/usr/local/bin/valmin-entrypoint"}

// steamAppID is 03 §1.1's case-sensitive trap: the shipped scripts export "SteamAppId"
// (lowercase d) — "SteamAppID" is a different, inert variable.
const steamAppID = "SteamAppId=892970"

// LaunchSpec is everything BuildSpec needs about one instance. It is deliberately not
// store.Instance: this package imports nothing from internal/store (ports.go's UsedPorts
// follows the same rule), so the caller — the job that already holds the row and has
// decrypted the password (10 §3) — converts.
type LaunchSpec struct {
	InstanceID          string
	DataDir             string // host path (02 §5), used verbatim for bind sources
	BasePort            int
	ServerName          string
	WorldName           string
	Password            string // plaintext; BuildSpec never touches the envelope
	Public              bool
	Crossplay           bool
	CrossplayInstanceID string
	Preset              string
	Modifiers           string // JSON object per 04 §2, or ""
	ExtraArgs           string
	MemLimitMB          int
	CPULimit            *float64
}

// 08 §1's four labels. Named rather than spelled inline because reconciliation reads them
// back (08 §6.1 joins Docker to the DB on LabelInstanceID), and a typo on the read side
// would present every container the panel owns as an orphan.
const (
	LabelManaged    = "io.valmin.managed"
	LabelSchema     = "io.valmin.schema"
	LabelInstanceID = "io.valmin.instance.id"
	LabelBasePort   = "io.valmin.base-port"
)

// Labels is 08 §1's enumeration filter — immutable facts only. Anything renameable
// (name, state, world, mod set, limits) lives in the DB, keyed on instance id.
func Labels(instanceID string, basePort int) map[string]string {
	return map[string]string{
		LabelManaged:    "true",
		LabelSchema:     "1",
		LabelInstanceID: instanceID,
		LabelBasePort:   strconv.Itoa(basePort),
	}
}

// ContainerName is 08 §1's human sugar. The panel never resolves a container by name —
// only by the io.valmin.instance.id label — so collisions here cost nothing but legibility.
func ContainerName(instanceID string) string {
	n := min(len(instanceID), 8)
	return "valmin-" + instanceID[:n]
}

// BuildSpec assembles the exact container 08 §5 fixes for one instance. image and
// stopTimeout come from config rather than LaunchSpec because they are panel-wide, not
// per-instance (08 §9's reversible/irreversible split runs through this function, not
// around it: labels, OpenStdin/StdinOnce/Tty and the UID are all set here and none of
// them varies by caller).
//
// `↯` Re-validates 03 §1.3's three rules (G2): the API handler that took this launch
// config already checked them, but this is the second call site — the one that still
// runs if a caller other than the handler reaches this function.
func BuildSpec(s *LaunchSpec, image string, stopTimeout time.Duration) (*runtime.ContainerSpec, error) {
	if v := ValidateLaunch(s.ServerName, s.WorldName, s.Password); len(v) > 0 {
		return nil, &InvalidLaunchConfigError{Violations: v}
	}

	args, err := launchArgs(s)
	if err != nil {
		return nil, err
	}

	return &runtime.ContainerSpec{
		Name:       ContainerName(s.InstanceID),
		Image:      image,
		Entrypoint: entrypoint,
		Cmd:        args,
		Env:        []string{steamAppID},
		Labels:     Labels(s.InstanceID, s.BasePort),
		User:       containerUser,

		// Set-once (A1, 07 §3): OpenStdin/StdinOnce/Tty can never be added later.
		OpenStdin: true,
		StdinOnce: false,
		TTY:       false,

		StopSignal:  "SIGINT",
		StopTimeout: stopTimeout,

		Binds: []runtime.Bind{
			{HostPath: s.DataDir + "/server", ContainerPath: "/opt/valheim/server"},
			{HostPath: s.DataDir + "/worlds", ContainerPath: "/opt/valheim/worlds"},
			{HostPath: s.DataDir + "/logs", ContainerPath: "/opt/valheim/logs"},
			// backups/ is deliberately not mounted — the game has no business in the
			// backup catalogue (08 §5).
		},
		Ports: []runtime.Port{
			{HostPort: s.BasePort, ContainerPort: s.BasePort, Proto: "udp"},
			{HostPort: s.BasePort + 1, ContainerPort: s.BasePort + 1, Proto: "udp"},
		},

		RestartPolicy: "unless-stopped",

		MemoryBytes: int64(s.MemLimitMB) << 20,
		NanoCPUs:    nanoCPUs(s.CPULimit),
	}, nil
}

// nanoCPUs converts a fractional core count to Docker's NanoCPUs unit. A nil limit
// means "generous or nothing" (03 §3.3) — zero is Docker's own "no limit".
func nanoCPUs(limit *float64) int64 {
	if limit == nil {
		return 0
	}
	return int64(*limit * 1e9)
}

// launchArgs builds the game's argv exactly per 03 §1.2/§1.3, as a typed struct's output
// rather than a concatenated string (D8) — every element lands as its own argv entry, so
// nothing here is ever re-parsed by a shell.
func launchArgs(s *LaunchSpec) ([]string, error) {
	args := []string{
		"-nographics", "-batchmode",
		"-name", s.ServerName,
		"-port", strconv.Itoa(s.BasePort),
		"-world", s.WorldName,
		"-password", s.Password,
		"-public", publicFlag(s.Public),
		"-savedir", "/opt/valheim/worlds",
	}

	if s.Crossplay {
		args = append(args, "-crossplay")
	}
	// -instanceid is set unconditionally, whether or not crossplay is on, and never
	// regenerated (A5, 03 §1.4) — the caller is the one place that owns that guarantee.
	args = append(args, "-instanceid", s.CrossplayInstanceID)

	if s.Preset != "" {
		args = append(args, "-preset", s.Preset)
	}

	mods, err := modifierArgs(s.Modifiers)
	if err != nil {
		return nil, err
	}
	args = append(args, mods...)

	// extra_args is the admin-only escape hatch (D15) — already gated by authz at the
	// point it was written, so it is trusted here and split into its own argv elements.
	if extra := strings.Fields(s.ExtraArgs); len(extra) > 0 {
		args = append(args, extra...)
	}

	return args, nil
}

func publicFlag(public bool) string {
	if public {
		return "1"
	}
	return "0"
}

// modifierArgs decodes 04 §2's JSON object into repeated "-modifier key value" pairs,
// sorted by key so the argv a given instance produces is stable across runs.
func modifierArgs(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var mods map[string]string
	if err := json.Unmarshal([]byte(raw), &mods); err != nil {
		return nil, fmt.Errorf("decode modifiers: %w", err)
	}
	keys := make([]string, 0, len(mods))
	for k := range mods {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys)*3)
	for _, k := range keys {
		args = append(args, "-modifier", k, mods[k])
	}
	return args, nil
}

// InvalidLaunchConfigError reports that a LaunchSpec reaching BuildSpec still violates
// 03 §1.3 despite the handler's own check (G2) — a caller that is not the handler.
type InvalidLaunchConfigError struct {
	Violations []LaunchViolation
}

func (e *InvalidLaunchConfigError) Error() string {
	return fmt.Sprintf("invalid launch config: %d violation(s)", len(e.Violations))
}
