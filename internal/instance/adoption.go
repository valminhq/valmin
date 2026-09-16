package instance

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// ErrContainerMismatch reports that a labelled orphan does not match the managed container
// contract. Adoption refuses it because accepting an unverified spec is a path to Docker.
var ErrContainerMismatch = errors.New("container does not match the managed contract")

// ValidateAdoptionContainer verifies the observed spec and runtime-enforced security
// fields, then returns the immutable registration id carried by its command.
func ValidateAdoptionContainer(c *runtime.Container) (string, error) {
	if err := validateAdoptionIdentity(c); err != nil {
		return "", err
	}
	if err := validateAdoptionSecurity(c); err != nil {
		return "", err
	}
	return adoptionCrossplayID(c.Spec.Cmd)
}

func validateAdoptionIdentity(c *runtime.Container) error {
	if c == nil || c.Spec.Name == "" || c.ImageDefaults == nil {
		return adoptionMismatch("the container spec is unavailable")
	}
	labels := c.Spec.Labels
	if !validAdoptionLabels(labels, c.Labels) {
		return adoptionMismatch("the management labels are missing, unsupported, or inconsistent")
	}
	instanceID := labels[LabelInstanceID]
	basePort, err := strconv.Atoi(labels[LabelBasePort])
	if err != nil || !validAdoptionIdentityLabels(instanceID, basePort) {
		return adoptionMismatch("the identity labels are invalid")
	}
	if !validAdoptionRuntimeIdentity(c, instanceID) {
		return adoptionMismatch("the container identity does not match its labels")
	}

	wantHash := labels[LabelSpecHash]
	if wantHash == "" {
		return adoptionMismatch("the container has no spec hash")
	}
	return nil
}

func validAdoptionLabels(spec, reported map[string]string) bool {
	return maps.Equal(spec, reported) && spec[LabelManaged] == "true" && spec[LabelSchema] == "1"
}

func validAdoptionIdentityLabels(instanceID string, basePort int) bool {
	return instanceID != "" && !strings.Contains(instanceID, "..") &&
		!strings.ContainsAny(instanceID, `/\`) && basePort > 0 && basePort <= 65534
}

func validAdoptionRuntimeIdentity(c *runtime.Container, instanceID string) bool {
	return c.ID != "" && c.Name == c.Spec.Name && c.Image == c.Spec.Image &&
		c.Name == ContainerName(instanceID)
}

func validateAdoptionSecurity(c *runtime.Container) error {
	security := c.Security
	if len(security.CapAdd) != 0 || len(security.CapDrop) != 1 ||
		!strings.EqualFold(security.CapDrop[0], "ALL") {
		return adoptionMismatch("the capability set is not empty")
	}
	if len(security.SecurityOpt) != 1 ||
		(security.SecurityOpt[0] != "no-new-privileges" &&
			security.SecurityOpt[0] != "no-new-privileges:true") {
		return adoptionMismatch("no-new-privileges is not enforced")
	}
	if security.Privileged || security.ReadonlyRootfs || security.NonBindMount ||
		security.MemorySwap != c.Spec.MemoryBytes {
		return adoptionMismatch("the host security configuration differs from the managed contract")
	}
	for _, hostIP := range security.HostIPs {
		if hostIP != "" {
			return adoptionMismatch("a published port is restricted to a foreign host address")
		}
	}
	return nil
}

func adoptionCrossplayID(args []string) (string, error) {
	var value string
	for i, arg := range args {
		if arg != "-instanceid" {
			continue
		}
		if value != "" || i+1 >= len(args) || args[i+1] == "" {
			return "", adoptionMismatch("the crossplay instance id is missing or ambiguous")
		}
		value = args[i+1]
	}
	if value == "" {
		return "", adoptionMismatch("the crossplay instance id is missing or ambiguous")
	}
	return value, nil
}

// ValidateAdoptionLaunch proves that the admin-supplied launch settings reproduce the exact
// spec hash on the orphan. The password participates in the hash but never leaves this call.
func ValidateAdoptionLaunch(
	c *runtime.Container, launch *LaunchSpec, image, network string, stopTimeout time.Duration,
) error {
	if _, err := ValidateAdoptionContainer(c); err != nil {
		return err
	}
	expected, err := BuildSpec(launch, image, network, stopTimeout)
	if err != nil {
		return err
	}
	// Named before the hash, which every field feeds into: a container from before the panel
	// had a game network fails on the hash alone, and that reads as wrong launch settings.
	if expected.Network != c.Spec.Network {
		return adoptionMismatch(fmt.Sprintf(
			"the container is on network %q and this panel creates instances on %q (A9)",
			c.Spec.Network, expected.Network))
	}
	if expected.Labels[LabelSpecHash] != c.Labels[LabelSpecHash] {
		return adoptionMismatch("the supplied launch settings do not describe this container")
	}
	if !adoptionSpecsEqual(expected, &c.Spec, c.ImageDefaults) {
		return adoptionMismatch("the observed container differs from the supplied launch settings")
	}
	return nil
}

func adoptionSpecsEqual(
	expected, observed *runtime.ContainerSpec, defaults *runtime.ContainerImageDefaults,
) bool {
	effective := *expected
	effective.Env = slices.Clone(expected.Env)
	for _, inherited := range defaults.Env {
		key, _, _ := strings.Cut(inherited, "=")
		found := false
		for _, explicit := range expected.Env {
			explicitKey, _, _ := strings.Cut(explicit, "=")
			if explicitKey == key {
				found = true
				break
			}
		}
		if !found {
			effective.Env = append(effective.Env, inherited)
		}
	}
	effective.Labels = maps.Clone(expected.Labels)
	for key, value := range defaults.Labels {
		if _, exists := effective.Labels[key]; !exists {
			effective.Labels[key] = value
		}
	}
	effective.Binds = sortedAdoptionBinds(effective.Binds)
	actual := *observed
	actual.Binds = sortedAdoptionBinds(actual.Binds)
	return reflect.DeepEqual(effective, actual)
}

func sortedAdoptionBinds(binds []runtime.Bind) []runtime.Bind {
	binds = slices.Clone(binds)
	compare := func(a, b runtime.Bind) int {
		return cmp.Or(strings.Compare(a.ContainerPath, b.ContainerPath),
			strings.Compare(a.HostPath, b.HostPath))
	}
	slices.SortFunc(binds, compare)
	return binds
}

func adoptionMismatch(reason string) error {
	return fmt.Errorf("%s: %w", reason, ErrContainerMismatch)
}
