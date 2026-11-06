package instance

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/runtime"
)

func validSpec() *LaunchSpec {
	return &LaunchSpec{
		InstanceID:          "0198abc1-2345-7000-8000-000000000001",
		DataDir:             "/srv/valmin/instances/inst-1",
		BasePort:            2456,
		ServerName:          "My Server",
		WorldName:           "MainWorld",
		Password:            "hunter2",
		CrossplayInstanceID: "cp-inst-1",
		MemLimitMB:          4096,
	}
}

func TestLabelsCarryOnlyImmutableFacts(t *testing.T) {
	got := Labels("abc123", 2456)
	want := map[string]string{
		"io.valmin.managed":     "true",
		"io.valmin.schema":      "1",
		"io.valmin.instance.id": "abc123",
		"io.valmin.base-port":   "2456",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestContainerNameIsHumanSugarNotAnIdentifier(t *testing.T) {
	if got := ContainerName("0198abc1-2345-7000-8000-000000000001"); got !=
		"valmin-0198abc1-2345-7000-8000-000000000001" {
		t.Errorf("got %q, want the whole id", got)
	}
	// A short id must not panic (defensive, not a shape the store ever produces).
	if got := ContainerName("ab"); got != "valmin-ab" {
		t.Errorf("got %q, want valmin-ab", got)
	}
}

// TestContainerNameSeparatesInstancesCreatedTogether asserts two instances created in the
// same millisecond get distinct container names. Docker refuses a duplicate name, so a
// collision fails the second instance at Create.
func TestContainerNameSeparatesInstancesCreatedTogether(t *testing.T) {
	// Identical timestamp halves, different random tails — what store.NewID produces for two
	// instances created back to back.
	a := ContainerName("01a06934-eb8c-7062-9bee-14cc095f936c")
	b := ContainerName("01a06934-eb8f-7c68-8d81-6baa642cd8bb")
	if a == b {
		t.Errorf("two instances created in the same millisecond share the container name %q", a)
	}
}

// TestBuildSpecCarriesTheSetOnceProperties guards A1/07 §3: OpenStdin, StdinOnce and Tty
// cannot be added to an existing container, so BuildSpec must set them on every call, not
// only when the caller remembers to ask.
func TestBuildSpecCarriesTheSetOnceProperties(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.OpenStdin || spec.StdinOnce || spec.TTY {
		t.Errorf("OpenStdin=%v StdinOnce=%v TTY=%v, want true/false/false", spec.OpenStdin, spec.StdinOnce, spec.TTY)
	}
	if spec.User != "10000:10000" {
		t.Errorf("User = %q, want 10000:10000", spec.User)
	}
	if spec.StopSignal != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", spec.StopSignal)
	}
	if spec.StopTimeout != 120*time.Second {
		t.Errorf("StopTimeout = %v, want 120s", spec.StopTimeout)
	}
	if spec.RestartPolicy != "unless-stopped" {
		t.Errorf("RestartPolicy = %q, want unless-stopped", spec.RestartPolicy)
	}
}

// The command channel is reached on the network the container is created with, and the spec
// hash covers it, so a changed network reaches Docker only through a rebuild (A9).
func TestBuildSpecCarriesTheGameNetwork(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Network != "valmin-games" {
		t.Errorf("Network = %q, want valmin-games", spec.Network)
	}
	if spec.NetworkDisabled {
		t.Error("NetworkDisabled is set on a game container")
	}

	other, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-other", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Labels[LabelSpecHash] == other.Labels[LabelSpecHash] {
		t.Error("two networks produce the same spec hash, so a move would never rebuild")
	}
}

// The Docker log is capped, and the cap feeds the spec hash, so a container created without
// it is rebuilt at its next start.
func TestBuildSpecCapsTheDockerLog(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.LogMaxBytes != 10<<20 || spec.LogMaxFiles != 2 {
		t.Errorf("log limit = %d bytes x %d files, want 10 MiB x 2", spec.LogMaxBytes, spec.LogMaxFiles)
	}

	uncapped := *spec
	uncapped.Labels = maps.Clone(spec.Labels)
	delete(uncapped.Labels, LabelSpecHash)
	uncapped.LogMaxBytes, uncapped.LogMaxFiles = 0, 0
	hash, err := specHash(&uncapped)
	if err != nil {
		t.Fatal(err)
	}
	if hash == spec.Labels[LabelSpecHash] {
		t.Error("an uncapped container hashes like a capped one, so it would never be rebuilt")
	}
}

func TestBuildSpecLabelsCarryTheInstanceAndPort(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Labels["io.valmin.instance.id"] != "0198abc1-2345-7000-8000-000000000001" {
		t.Errorf("labels = %v, missing the instance id", spec.Labels)
	}
	if spec.Labels["io.valmin.base-port"] != "2456" {
		t.Errorf("labels = %v, want base-port 2456", spec.Labels)
	}
}

func TestBuildSpecBindsExcludeBackups(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Binds) != 3 {
		t.Fatalf("got %d binds, want 3 (server, worlds, logs)", len(spec.Binds))
	}
	for _, b := range spec.Binds {
		if b.ContainerPath == "/opt/valheim/backups" {
			t.Error("backups/ must never be mounted into the game container (08 §5)")
		}
	}
}

func TestBuildSpecPortsAreTheConsecutiveUDPPair(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ host, container int }{{2456, 2456}, {2457, 2457}}
	if len(spec.Ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(spec.Ports))
	}
	for i, p := range spec.Ports {
		if p.Proto != "udp" || p.HostPort != want[i].host || p.ContainerPort != want[i].container {
			t.Errorf("port %d = %+v, want udp %d", i, p, want[i].host)
		}
	}
}

func TestBuildSpecMemoryAndCPU(t *testing.T) {
	cpu := 2.5
	s := validSpec()
	s.CPULimit = &cpu
	spec, err := BuildSpec(s, "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.MemoryBytes != 4096<<20 {
		t.Errorf("MemoryBytes = %d, want %d", spec.MemoryBytes, 4096<<20)
	}
	if spec.NanoCPUs != 2_500_000_000 {
		t.Errorf("NanoCPUs = %d, want 2_500_000_000", spec.NanoCPUs)
	}
}

func TestBuildSpecNilCPULimitIsUnlimited(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if spec.NanoCPUs != 0 {
		t.Errorf("NanoCPUs = %d, want 0 (no limit)", spec.NanoCPUs)
	}
}

// TestBuildSpecRejectsAnInvalidLaunchConfig is G2: a caller other than the API handler
// must still be stopped before a container is ever created.
func TestBuildSpecRejectsAnInvalidLaunchConfig(t *testing.T) {
	s := validSpec()
	s.Password = "abc" // shorter than MinPasswordLength
	_, err := BuildSpec(s, "valmin/valheim:dev", "valmin-games", 120*time.Second)

	var invalid *InvalidLaunchConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *InvalidLaunchConfigError", err)
	}
	if len(invalid.Violations) != 1 || invalid.Violations[0].Rule != RulePasswordTooShort {
		t.Errorf("violations = %+v, want just password_too_short", invalid.Violations)
	}
}

func TestBuildSpecRejectsUnsafeResourceLimits(t *testing.T) {
	cpu := -1.0
	s := validSpec()
	s.MemLimitMB = MinMemoryLimitMB - 1
	s.CPULimit = &cpu
	_, err := BuildSpec(s, "valmin/valheim:dev", "valmin-games", 120*time.Second)

	var invalid *InvalidResourceConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *InvalidResourceConfigError", err)
	}
	if len(invalid.Violations) != 2 {
		t.Errorf("violations = %+v, want memory and CPU", invalid.Violations)
	}
}

func TestLaunchArgsCarryTheThreeValidatedFieldsAndCrossplay(t *testing.T) {
	s := validSpec()
	s.Public = true
	s.Crossplay = true
	s.Preset = "hard"

	args, err := launchArgs(s)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"-nographics", "-batchmode",
		"-name", "My Server",
		"-port", "2456",
		"-world", "MainWorld",
		"-password", "hunter2",
		"-public", "1",
		"-savedir", "/opt/valheim/worlds",
		"-crossplay",
		"-instanceid", "cp-inst-1",
		"-preset", "hard",
	}
	if !slices.Equal(args, want) {
		t.Errorf("got  %v\nwant %v", args, want)
	}
}

// TestLaunchArgsBackups asserts -backups appears only for a count other than the game's default.
func TestLaunchArgsBackups(t *testing.T) {
	tests := []struct {
		backups int
		want    bool
	}{{0, false}, {Saves.Backups, false}, {10, true}}
	for _, tt := range tests {
		s := validSpec()
		s.Backups = tt.backups
		args, err := launchArgs(s)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.Index(args, "-backups")
		if got := i >= 0; got != tt.want {
			t.Errorf("Backups %d: -backups present = %v, want %v (%v)", tt.backups, got, tt.want, args)
		}
		if tt.want && args[i+1] != strconv.Itoa(tt.backups) {
			t.Errorf("Backups %d: -backups %s", tt.backups, args[i+1])
		}
	}
}

// TestLaunchArgsInstanceIDIsAlwaysSet is A5/03 §1.4: -instanceid is set whether or not
// crossplay is enabled, so toggling it on later never mints a new identity.
func TestLaunchArgsInstanceIDIsAlwaysSet(t *testing.T) {
	args, err := launchArgs(validSpec()) // Crossplay: false
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, a := range args {
		if a == "-instanceid" && i+1 < len(args) && args[i+1] == "cp-inst-1" {
			found = true
		}
		if a == "-crossplay" {
			t.Error("-crossplay must not appear when the instance has it disabled")
		}
	}
	if !found {
		t.Error("-instanceid missing with crossplay disabled")
	}
}

func TestLaunchArgsModifiersAreSortedForStableOutput(t *testing.T) {
	s := validSpec()
	s.Modifiers = `{"resources":"more","combat":"veryhard"}`

	args, err := launchArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-modifier")
	if i < 0 || i+5 >= len(args) {
		t.Fatalf("modifiers missing from %v", args)
	}
	got := args[i : i+6]
	want := []string{"-modifier", "combat", "veryhard", "-modifier", "resources", "more"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (sorted by key)", got, want)
	}
}

// TestLaunchArgsWorldKeysBecomeSetkey asserts that a world key in the modifiers map is passed
// as -setkey with no value, beside the axes passed as -modifier.
func TestLaunchArgsWorldKeysBecomeSetkey(t *testing.T) {
	s := validSpec()
	s.Modifiers = `{"nomap":"on","combat":"hard","fire":"on"}`

	args, err := launchArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-modifier")
	if i < 0 || i+6 >= len(args) {
		t.Fatalf("modifiers missing from %v", args)
	}
	got := args[i : i+7]
	want := []string{"-modifier", "combat", "hard", "-setkey", "fire", "-setkey", "nomap"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLaunchArgsMalformedModifiersIsAnError(t *testing.T) {
	s := validSpec()
	s.Modifiers = "not json"
	if _, err := launchArgs(s); err == nil {
		t.Error("want an error for malformed modifiers, got nil")
	}
}

// TestLaunchArgsExtraArgsAreSeparateArgvElements is D8: extra_args must never be
// concatenated into one string a shell could reinterpret — each field is its own argv
// entry, exactly as if a user had typed them at a real command line.
func TestLaunchArgsExtraArgsAreSeparateArgvElements(t *testing.T) {
	s := validSpec()
	s.ExtraArgs = "-logFile /opt/valheim/logs/game.log"

	args, err := launchArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "-logFile") || !slices.Contains(args, "/opt/valheim/logs/game.log") {
		t.Errorf("extra_args not split into argv: %v", args)
	}
}

// A container from before the panel had a game network fails adoption on the hash, which
// reads as wrong launch settings. The refusal names the networks instead.
func TestValidateAdoptionLaunchNamesANetworkMismatch(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	orphan := &runtime.Container{
		ID: "c1", Name: spec.Name, Image: spec.Image, Labels: spec.Labels,
		Spec: *spec,
		Security: runtime.ContainerSecurity{
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			MemorySwap:  spec.MemoryBytes,
		},
		ImageDefaults: &runtime.ContainerImageDefaults{},
	}

	err = ValidateAdoptionLaunch(orphan, validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err == nil {
		t.Fatal("a container on another network was adopted")
	}
	if !strings.Contains(err.Error(), "valmin-games") {
		t.Errorf("refusal does not name the network this panel uses:\n%v", err)
	}
}

// A sandbox puts the container under its runtime with the game resolv.conf mounted read-only,
// and either change moves the spec hash, so the next start rebuilds the container.
func TestBuildSpecCarriesTheSandbox(t *testing.T) {
	plain, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := validSpec()
	s.Sandbox = Sandbox{Runtime: "runsc", ResolvConf: "/srv/valmin/" + config.GameResolvFile}
	sandboxed, err := BuildSpec(s, "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if sandboxed.OCIRuntime != "runsc" {
		t.Errorf("OCIRuntime = %q, want runsc", sandboxed.OCIRuntime)
	}
	resolv := runtime.Bind{
		HostPath: "/srv/valmin/" + config.GameResolvFile, ContainerPath: "/etc/resolv.conf", ReadOnly: true,
	}
	if !slices.Contains(sandboxed.Binds, resolv) {
		t.Errorf("Binds = %+v, want the read-only resolv.conf %+v", sandboxed.Binds, resolv)
	}
	if slices.ContainsFunc(plain.Binds, func(b runtime.Bind) bool { return b.ContainerPath == "/etc/resolv.conf" }) {
		t.Errorf("Binds = %+v, want no resolv.conf without a sandbox", plain.Binds)
	}
	if plain.Labels[LabelSpecHash] == sandboxed.Labels[LabelSpecHash] {
		t.Error("a sandboxed spec hashes like a plain one, so moving to a runtime would never rebuild")
	}
}

// The default runtime leaves no trace in the hashed JSON, so containers created before a
// runtime could be named keep their hash.
func TestDefaultRuntimeStaysOutOfTheSpecHash(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "OCIRuntime") {
		t.Errorf("the hashed spec names a default runtime:\n%s", raw)
	}
}

// An orphan on another runtime fails adoption with a refusal naming both runtimes.
func TestValidateAdoptionLaunchNamesARuntimeMismatch(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	orphan := &runtime.Container{
		ID: "c1", Name: spec.Name, Image: spec.Image, Labels: spec.Labels,
		Spec: *spec,
		Security: runtime.ContainerSecurity{
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			MemorySwap:  spec.MemoryBytes,
		},
		ImageDefaults: &runtime.ContainerImageDefaults{},
	}

	launch := validSpec()
	launch.Sandbox = Sandbox{Runtime: "runsc"}
	err = ValidateAdoptionLaunch(orphan, launch, "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err == nil {
		t.Fatal("a container on the default runtime was adopted by a panel that runs runsc")
	}
	if !strings.Contains(err.Error(), "Docker's default") || !strings.Contains(err.Error(), `"runsc"`) {
		t.Errorf("refusal does not name both runtimes:\n%v", err)
	}
}

// A daemon whose default runtime is not runc reads that name back on a container created
// without one; it matches a panel that names no runtime, and not one that names another.
func TestAdoptionComparesTheRuntimeOnlyWhenOneIsNamed(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	observed := *spec
	observed.OCIRuntime = "nvidia"
	defaults := &runtime.ContainerImageDefaults{}

	if !adoptionSpecsEqual(spec, &observed, defaults) {
		t.Error("a container on the daemon's default runtime differs from a spec naming none")
	}
	named := *spec
	named.OCIRuntime = "runsc"
	if adoptionSpecsEqual(&named, &observed, defaults) {
		t.Error("a container on nvidia matches a spec naming runsc")
	}
}

// The spec hash of a plain game container is pinned, so a field added to every spec, which
// would recreate every existing container on its next start, fails here first.
func TestThePlainSpecHashIsStable(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const want = "67de2196a0858607031574003cd5e1668187daee1a3c4dadc8b0e25b0bb24e96"
	if got := spec.Labels[LabelSpecHash]; got != want {
		t.Errorf("plain spec hash = %s, want %s", got, want)
	}
}

// A panel that names no runtime adopts an orphan whose daemon reports its default runtime
// under another name than runc.
func TestValidateAdoptionLaunchAcceptsTheDaemonsDefaultRuntime(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", "valmin-games", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	orphan := &runtime.Container{
		ID: "c1", Name: spec.Name, Image: spec.Image, Labels: spec.Labels,
		Spec: *spec,
		Security: runtime.ContainerSecurity{
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			MemorySwap:  spec.MemoryBytes,
		},
		ImageDefaults: &runtime.ContainerImageDefaults{},
	}
	orphan.Spec.OCIRuntime = "nvidia"

	if err := ValidateAdoptionLaunch(
		orphan,
		validSpec(),
		"valmin/valheim:dev",
		"valmin-games",
		120*time.Second,
	); err != nil {
		t.Errorf("ValidateAdoptionLaunch = %v, want the orphan adopted", err)
	}
}
