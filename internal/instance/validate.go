package instance

import "strings"

// MinPasswordLength is 03 §1.3's floor on the game password — a different, lower bar than
// 06 §1 ADR-054's 8-character panel account floor. This one is the server's own refusal
// threshold, not a policy pick.
const MinPasswordLength = 5

// LaunchRule names one of 03 §1.3's three validated rules — the cause of most "server
// won't boot" reports. Kept as a small closed set of reasons rather than a bare string so
// the API layer can map each to its own field code without parsing prose.
type LaunchRule string

const (
	RulePasswordTooShort  LaunchRule = "password_too_short"
	RulePasswordInName    LaunchRule = "password_in_name"
	RuleWorldSameAsServer LaunchRule = "world_same_as_server"
)

// LaunchViolation is one broken rule, named without any HTTP or presentation concern —
// internal/api translates this into 11 §2.4's field-error shape.
type LaunchViolation struct {
	Field string
	Rule  LaunchRule
}

// ValidateLaunch enforces 03 §1.3's three rules, exactly. 08 §5.1 re-checks the same three
// at container creation (G2) against a caller that is not this validation's handler; this
// copy exists to protect the user with a fast, readable 422 before anything is created.
func ValidateLaunch(serverName, worldName, password string) []LaunchViolation {
	var v []LaunchViolation
	if len(password) < MinPasswordLength {
		v = append(v, LaunchViolation{"password", RulePasswordTooShort})
	}
	if password != "" && (strings.Contains(serverName, password) || strings.Contains(worldName, password)) {
		v = append(v, LaunchViolation{"password", RulePasswordInName})
	}
	if serverName != "" && serverName == worldName {
		v = append(v, LaunchViolation{"world_name", RuleWorldSameAsServer})
	}
	return v
}
