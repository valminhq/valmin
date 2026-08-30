package instance

import (
	"errors"
	"slices"
	"testing"
	"time"
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
	if got := ContainerName("0198abc1-2345-7000-8000-000000000001"); got != "valmin-0198abc1" {
		t.Errorf("got %q, want valmin-0198abc1", got)
	}
	// A short id must not panic (defensive, not a shape the store ever produces).
	if got := ContainerName("ab"); got != "valmin-ab" {
		t.Errorf("got %q, want valmin-ab", got)
	}
}

// TestBuildSpecCarriesTheSetOnceProperties guards A1/07 §3: OpenStdin, StdinOnce and Tty
// cannot be added to an existing container, so BuildSpec must set them on every call, not
// only when the caller remembers to ask.
func TestBuildSpecCarriesTheSetOnceProperties(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", 120*time.Second)
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

func TestBuildSpecLabelsCarryTheInstanceAndPort(t *testing.T) {
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", 120*time.Second)
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
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", 120*time.Second)
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
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", 120*time.Second)
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
	spec, err := BuildSpec(s, "valmin/valheim:dev", 120*time.Second)
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
	spec, err := BuildSpec(validSpec(), "valmin/valheim:dev", 120*time.Second)
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
	_, err := BuildSpec(s, "valmin/valheim:dev", 120*time.Second)

	var invalid *InvalidLaunchConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want *InvalidLaunchConfigError", err)
	}
	if len(invalid.Violations) != 1 || invalid.Violations[0].Rule != RulePasswordTooShort {
		t.Errorf("violations = %+v, want just password_too_short", invalid.Violations)
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
