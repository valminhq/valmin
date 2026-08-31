package middleware

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/store"
)

// Layer is one link of the chain, outermost first.
type Layer func(http.Handler) http.Handler

// Config is what the chain needs from the operator's settings.
type Config struct {
	// TrustedProxies is empty by default, which means X-Forwarded-For is ignored (D9).
	TrustedProxies []netip.Prefix
	// ExternalURL is the origin every browser request must carry (11 §6.1).
	ExternalURL *url.URL
	// BodyLimit is the default cap; routes that accept a world raise it (11 §8.3).
	BodyLimit int64
	// Keeper derives the CSRF subkey (10 §3.2).
	Keeper *crypto.Keeper
	// PerIP is the chain-wide limiter of 11 §5.1 row 8. The tighter per-route limits
	// 11 §7 puts on login, /setup and invite redemption are the handlers' own.
	PerIP *Limiter
	// PerUser is row 11: generous, a bug and flood guard rather than a business rule.
	PerUser *Limiter
	// Bootstrap is row 7 — 503 setup_required until the first admin exists (10 §6).
	Bootstrap *BootstrapGate
	// Auth resolves a session cookie to a user — row 9. Nil is valid: a router built
	// before WP-09's Sessions type exists (tests, early wiring) simply authenticates
	// nobody.
	Auth SessionAuthenticator
}

// Chain is 11 §5.1, outermost first. The order is a correctness property, not a style
// preference: the client IP is resolved before rate limiting and before anything that
// writes audit_log, the body is capped before anything parses it, and the origin check
// runs before a session cookie is ever read.
//
// Authorization is absent by design. Route-pattern authorization middleware fails open —
// a route added later that matches no pattern is unprotected and nothing reports it — so
// every handler calls Can() in its own body instead (ADR-037, D1).
func Chain(cfg *Config) []Layer {
	chain := []Layer{
		Recover,
		RequestID,
		ClientIP(cfg.TrustedProxies),
		SecurityHeaders,
		BodyLimit(cfg.BodyLimit),
		Origin(cfg.ExternalURL),
	}
	if cfg.Bootstrap != nil {
		chain = append(chain, SetupGate(cfg.Bootstrap))
	}
	chain = append(chain, RateLimit(cfg.PerIP))
	if cfg.Auth != nil {
		chain = append(chain, SessionAuth(cfg.Auth))
	}
	chain = append(chain, CSRF(cfg.Keeper), AuthRateLimit(cfg.PerUser))
	return chain
}

// Apply wraps h in layers, so layers[0] is the outermost and runs first.
func Apply(h http.Handler, layers []Layer) http.Handler {
	for i := len(layers) - 1; i >= 0; i-- {
		h = layers[i](h)
	}
	return h
}

// Recover is the outermost layer. Without it a panic anywhere below returns a bare 500
// from net/http with no request id and no log line (11 §5.1 row 1).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func(ctx context.Context) {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && stderrors.Is(err, http.ErrAbortHandler) {
				// net/http's own signal that a handler gave up on the connection. It is
				// not a bug and must reach the server, which logs nothing for it.
				panic(v)
			}
			slog.ErrorContext(ctx, "handler panicked",
				slog.Any("panic", v),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("stack", string(debug.Stack())))
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(fmt.Errorf("panic: %v", v)))
		}(r.Context())
		next.ServeHTTP(w, r)
	})
}

// RequestID stamps X-Request-Id on every response, success or failure, and puts it in the
// request context. It is how an operator ties "it said something went wrong" to the log
// line that holds the real error (11 §2.1).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := store.NewID()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// SecurityHeaders sets the unconditional response headers and marks API responses
// uncacheable, so an authenticated payload never lands in a browser or proxy cache.
//
// There is deliberately no Access-Control-Allow-* header here or anywhere else: the SPA is
// served by the binary that serves the API, nothing legitimate is cross-origin, and a knob
// for it is the knob that gets set to * at 2 a.m. (D3, ADR-036).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// BodyLimit caps the request body before anything parses it, so an oversized body is
// refused rather than buffered (11 §5.1 row 5). A declared length over the cap is rejected
// outright; anything else is capped at the reader, which catches a chunked body that never
// declared one.
// isUpload names the routes 11 §8.3 exempts from the JSON cap: "body limits are per route,
// not global — 1 MiB for ordinary JSON, larger for world import and backup upload".
//
// `↯` Exempt here does not mean unbounded. The handler applies its own, far larger cap as it
// streams to disk; this only stops a 1 MiB rule written for JSON from rejecting a world
// before any handler sees it.
func isUpload(p string) bool { return strings.HasSuffix(p, "/worlds/import") }

func BodyLimit(n int64) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUpload(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > n {
				apierr.Write(w, r, apierr.New(apierr.PayloadTooLarge).With("limit_bytes", n))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
