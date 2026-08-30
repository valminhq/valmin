package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// SessionCookie is where the session token lives (10 §4.1). Distinct from CSRFCookie: this
// one is HttpOnly, that one has to be readable by JS.
const SessionCookie = "valmin_session"

// SetSessionCookie writes the session cookie on login. Secure is unconditional — the panel
// is never served over plain HTTP (02 §5), and a dev escape hatch here is the flag someone
// ships with. Expires mirrors the session's own absolute expiry: the browser drops it at
// the same moment the server would refuse it anyway, which is a courtesy, not the boundary
// — that boundary is enforced server-side regardless of what the cookie claims.
func SetSessionCookie(w http.ResponseWriter, value string, absoluteExpiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: value, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		Expires: absoluteExpiresAt,
	})
}

// ClearSessionCookie expires the cookie on logout.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// SessionAuthenticator resolves a cookie value to the user and session id it names, or
// (nil, "", nil) for no session — never an error for "not authenticated", only for a
// genuine failure to ask the question. Defined here, the consumer, per 06 §4; the auth
// package's Sessions type satisfies it without importing net/http.
type SessionAuthenticator interface {
	Authenticate(ctx context.Context, cookieValue string) (*store.User, string, error)
}

// SessionAuth is chain row 9: session authentication resolves who you are and puts a user
// in context. It is not authorization — every handler still calls Can() (ADR-037) — and it
// does not reject an unauthenticated request itself; a route that requires a session finds
// none in context and answers 401 on its own terms.
func SessionAuth(auth SessionAuthenticator) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(SessionCookie)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}

			u, sessionID, err := auth.Authenticate(r.Context(), cookie.Value)
			if err != nil {
				// A store failure here is not the caller's fault, but it also should not
				// stop the request cold — CSRF and the handler both fail closed on a nil
				// user, so this degrades to "unauthenticated" rather than a hard 500.
				slog.ErrorContext(r.Context(), "session lookup failed", slog.Any("error", err))
				next.ServeHTTP(w, r)
				return
			}
			if u == nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := WithUser(r.Context(), u)
			ctx = WithSessionID(ctx, sessionID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
