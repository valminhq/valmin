// This file is the negative control for the exemption list: it holds a handler whose name
// collides with an exempted one in another file. The exemption is keyed by file and function
// together, so this one must still be reported.
package authzfixture

import "net/http"

// status collides by name with the exempted public_status.go:status and must not inherit its
// exemption.
func status(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
