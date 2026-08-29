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
