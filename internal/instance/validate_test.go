package instance

import "testing"

func hasRule(v []LaunchViolation, field string, rule LaunchRule) bool {
	for _, e := range v {
		if e.Field == field && e.Rule == rule {
			return true
		}
	}
	return false
}

// TestValidateLaunchAcceptsAGoodConfiguration is 05 M1's negative case: nothing flagged.
func TestValidateLaunchAcceptsAGoodConfiguration(t *testing.T) {
	if v := ValidateLaunch("My Server", "MyWorld", "correct-horse"); len(v) != 0 {
		t.Errorf("got violations %+v, want none", v)
	}
}

// TestValidateLaunchPasswordTooShort is 03 §1.3 rule 1.
func TestValidateLaunchPasswordTooShort(t *testing.T) {
	v := ValidateLaunch("My Server", "MyWorld", "abcd")
	if !hasRule(v, "password", RulePasswordTooShort) {
		t.Errorf("got %+v, want password_too_short", v)
	}
}

// TestValidateLaunchPasswordInName is 03 §1.3 rule 2, and 05 M1's own acceptance example:
// password="server" with server_name="my server" is a violation naming the field.
func TestValidateLaunchPasswordInName(t *testing.T) {
	v := ValidateLaunch("my server", "MyWorld", "server")
	if !hasRule(v, "password", RulePasswordInName) {
		t.Errorf("got %+v, want password_in_name", v)
	}
}

func TestValidateLaunchPasswordInWorldName(t *testing.T) {
	v := ValidateLaunch("My Server", "myworld", "world")
	if !hasRule(v, "password", RulePasswordInName) {
		t.Errorf("got %+v, want password_in_name", v)
	}
}

// TestValidateLaunchWorldSameAsServer is 03 §1.3 rule 3.
func TestValidateLaunchWorldSameAsServer(t *testing.T) {
	v := ValidateLaunch("Valheim", "Valheim", "correct-horse")
	if !hasRule(v, "world_name", RuleWorldSameAsServer) {
		t.Errorf("got %+v, want world_same_as_server", v)
	}
}

// TestValidateLaunchEmptyPasswordDoesNotFalsePositiveOnSubstring guards the
// strings.Contains(x, "") == true trap: an empty password already fails rule 1 and must
// not also spuriously trigger rule 2.
func TestValidateLaunchEmptyPasswordDoesNotFalsePositiveOnSubstring(t *testing.T) {
	v := ValidateLaunch("My Server", "MyWorld", "")
	if hasRule(v, "password", RulePasswordInName) {
		t.Errorf("got %+v, empty password must not trigger password_in_name", v)
	}
	if !hasRule(v, "password", RulePasswordTooShort) {
		t.Errorf("got %+v, want password_too_short for an empty password", v)
	}
}

func TestValidateLaunchReportsEveryViolationAtOnce(t *testing.T) {
	// "alh" is short (rule 1), a substring of "Valheim" (rule 2), and the name equals the
	// world (rule 3) — all three violations from one configuration, at once (11 §2.4).
	v := ValidateLaunch("Valheim", "Valheim", "alh")
	if len(v) != 3 {
		t.Fatalf("got %d violations %+v, want 3 (all at once, 11 §2.4)", len(v), v)
	}
}
