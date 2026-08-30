// Package authzfixture is a fixture for TestDetectorFiresOnMissingCan. It lives under
// testdata so the Go tool never builds it. handlerWithoutCan is intentionally
// unguarded; do not add a Can call to it.
package authzfixture

import "net/http"

func Can(any, string, string) bool { return true }

type authz struct{}

func (a *authz) Can(any, string, string) bool { return true }

// handlerWithoutCan is the violation the detector must catch.
func handlerWithoutCan(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handlerWithCan calls Can as a package-level function.
func handlerWithCan(w http.ResponseWriter, r *http.Request) {
	if !Can(r.Context(), "instance.view", "abc") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlerWithMethodCan calls it as a method, which is the real shape.
func handlerWithMethodCan(w http.ResponseWriter, r *http.Request) {
	var a authz
	if !a.Can(r.Context(), "instance.start", "abc") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// notAHandler must not be flagged: wrong signature, so it is not a route target.
func notAHandler(s string) string { return s }

// helperReturningAValue must not be flagged either: it takes the handler pair but hands
// something back, so it is a helper rather than a route.
func helperReturningAValue(w http.ResponseWriter, r *http.Request) bool {
	_ = w
	return r != nil
}

type user struct{ Role string }

// handlerBranchingOnCallerRole is the violation TestRoleDetectorFires must catch: it
// decides from the caller's own role instead of asking Can (09 §4). The variable is
// deliberately named `caller`, the one name the detector keys on.
func handlerBranchingOnCallerRole(w http.ResponseWriter, r *http.Request) {
	var caller user
	if !Can(r.Context(), "instance.view", "abc") {
		return
	}
	if caller.Role == "admin" {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// handlerValidatingATargetRole must NOT be flagged: comparing a *different* user's role —
// a request body's requested value, a target row's current value — is ordinary field
// access, not the caller standing in for Can(). Only `caller.Role` is the bug.
func handlerValidatingATargetRole(w http.ResponseWriter, r *http.Request) {
	if !Can(r.Context(), "users.manage", "") {
		return
	}
	var body, current user
	if body.Role != "admin" && body.Role != "member" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	if body.Role != current.Role {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
