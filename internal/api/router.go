// Package api holds the HTTP handlers and their DTOs. Each handler calls authz.Can in
// its own body (ADR-037).
//
// Specification: 11, 04 §3.
package api

import (
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
)

// timeoutBody is what http.TimeoutHandler writes when a handler overruns. Fixed at
// construction, so it carries no request id, which the X-Request-Id header on the same
// response does. TimeoutHandler answers 503, hence unavailable rather than 11 §2.5's timeout,
// which is reserved for an upstream that did not answer.
const timeoutBody = `{"error":{"code":"unavailable",` +
	`"message":"The panel cannot do that right now.","request_id":""}}`

// Router is the panel's HTTP surface: the probes outside the chain, the API subtree behind
// it, and the rule that /api never falls through to the SPA.
type Router struct {
	mux    *http.ServeMux
	api    *http.ServeMux
	chain  []middleware.Layer
	within time.Duration
	spa    http.Handler
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

// handle registers an API route behind the request timeout of 11 §8.1. The pattern is the
// full path, method included: "GET /api/v1/instances".
func (rt *Router) handle(pattern string, h http.Handler) {
	rt.api.Handle(pattern, http.TimeoutHandler(h, rt.within, timeoutBody))
}

// stream registers a long-lived route: the console socket, a backup download. It gets no write
// deadline, since a server-wide one would sever the console after thirty seconds (C12, 11 §8.1).
//
// X-Accel-Buffering keeps nginx from spooling the response to its own disk before sending it;
// everything else ignores the header.
func (rt *Router) stream(pattern string, h http.Handler) {
	rt.api.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Accel-Buffering", "no")
		h.ServeHTTP(w, r)
	}))
}

// dispatch hands the request to a registered API route, or answers 404 in the envelope, since
// http.ServeMux would otherwise answer with a bare text/plain string (11 §1.1). A path that
// exists under another method also reads as not_found (ADR-038).
//
// It asks the mux whether anything matched and lets the mux serve, rather than invoking the
// handler it hands back: only ServeHTTP binds the wildcards, so calling the handler directly
// leaves every r.PathValue empty.
func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	if _, pattern := rt.api.Handler(r); pattern == "" {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	rt.api.ServeHTTP(w, r)
}
