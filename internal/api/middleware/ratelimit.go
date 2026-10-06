package middleware

import (
	"net/http"
	"strconv"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/ratelimit"
)

// Limiter is the shared token bucket used by request middleware.
type Limiter = ratelimit.Limiter

var NewLimiter = ratelimit.NewLimiter

// RateLimit is the unauthenticated per-IP limit of 11 §5.1 row 8, guarding login, /setup and
// invite redemption. It runs before the handler that hashes a password, since hashing first
// would make the limiter a memory amplifier rather than the control for one (D12).
func RateLimit(l *Limiter) Layer {
	return keyedRateLimit(l, func(r *http.Request) string {
		return ClientIPFrom(r.Context()).String()
	})
}

// AuthRateLimit is 11 §5.1 row 11, the authenticated per-user limit. Generous by design
// (11 §7), a bug and flood guard rather than a business rule, and sits below CSRF since it
// applies only once a session has resolved.
func AuthRateLimit(l *Limiter) Layer {
	return keyedRateLimit(l, func(r *http.Request) string {
		if u := UserFrom(r.Context()); u != nil {
			return u.ID
		}
		return ""
	})
}

func keyedRateLimit(l *Limiter, key func(*http.Request) string) Layer {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			if ok, retry := l.Allow(k); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
				apierr.Write(w, r, apierr.New(apierr.RateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
