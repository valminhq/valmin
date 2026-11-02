package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/store"
)

// SessionCookie is where the session token lives (10 §4.1). Distinct from CSRFCookie: this
// one is HttpOnly, that one has to be readable by JS.
const SessionCookie = "valmin_session"

// SetSessionCookie writes the session cookie on login. Secure is unconditional, the panel
// never being served over plain HTTP (02 §5). Expires mirrors the session's absolute expiry as
// a courtesy; the boundary itself is enforced server-side regardless of what the cookie
// claims.
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
// (nil, "", nil) for no session, never an error for "not authenticated". Defined here, the
// consumer, per 06 §4; auth.Sessions satisfies it without importing net/http.
type SessionAuthenticator interface {
	Authenticate(ctx context.Context, cookieValue string) (*store.User, string, error)
}

// SessionAuth is chain row 9: session authentication resolves who you are and puts a user in
// context. It is not authorization, since every handler still calls Can() (ADR-037), and does
// not reject an unauthenticated request itself; a route requiring a session finds none in
// context and answers 401 on its own terms.
//
// With proxy set, a username the trusted proxy names overrides the cookie: a session for a
// different user is ignored, and a GET or HEAD opens one for the named user. Other methods
// never open one, since a write's CSRF token could not match a session it had not seen.
func SessionAuth(sessions SessionAuthenticator, proxy *ProxyAuth) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, sessionID := cookieSession(r, sessions)
			if name := proxy.username(r); name != "" && (u == nil || u.Username != name) {
				u, sessionID = nil, ""
				if r.Method == http.MethodGet || r.Method == http.MethodHead {
					u, sessionID = proxy.signIn(w, r, name)
				}
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

// cookieSession resolves the request's session cookie, or returns no user when it carries
// none that is current.
func cookieSession(r *http.Request, sessions SessionAuthenticator) (u *store.User, sessionID string) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, ""
	}
	u, sessionID, err = sessions.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		// A store failure here is not the caller's fault, but it also should not
		// stop the request cold — CSRF and the handler both fail closed on a nil
		// user, so this degrades to "unauthenticated" rather than a hard 500.
		slog.ErrorContext(r.Context(), "session lookup failed", slog.Any("error", err))
		return nil, ""
	}
	return u, sessionID
}

// TrustedLogin opens a session for a user a trusted reverse proxy has already authenticated.
// auth.Sessions satisfies it.
type TrustedLogin interface {
	LoginTrusted(ctx context.Context, username, ip, userAgent string) (*auth.LoggedIn, error)
}

// ProxyAuth signs in the user a reverse proxy names in Header. The header is read only from
// a socket peer inside Trusted; from anywhere else it is ignored.
type ProxyAuth struct {
	Header  string
	Trusted []netip.Prefix
	Login   TrustedLogin
}

// username is the user the trusted proxy names on r, or "" when p is nil, r came from another
// peer or the header is absent.
func (p *ProxyAuth) username(r *http.Request) string {
	if p == nil || !isTrusted(peerAddr(r.RemoteAddr), p.Trusted) {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(p.Header))
}

// signIn opens a session for username and sets its cookie, or returns no user when the
// account is unknown, disabled or an admin. The header value is never logged, since an
// operator may point Header at one that carries a credential.
//
// ponytail: every trusted read without a session cookie opens one, so a client that drops
// cookies, or parallel first requests, open several; add a per-username limiter if rows pile up.
func (p *ProxyAuth) signIn(
	w http.ResponseWriter, r *http.Request, username string,
) (u *store.User, sessionID string) {
	ctx := r.Context()
	ip := ClientIPFrom(ctx).String()
	logged, err := p.Login.LoginTrusted(ctx, username, ip, r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrAccountDisabled),
		errors.Is(err, auth.ErrProxyAdmin):
		slog.DebugContext(ctx, "refuse proxy sign-in", slog.String("ip", ip), slog.Any("error", err))
		return nil, ""
	case err != nil:
		slog.ErrorContext(ctx, "proxy sign-in failed", slog.String("ip", ip), slog.Any("error", err))
		return nil, ""
	}
	SetSessionCookie(w, logged.Cookie, logged.AbsoluteExpiresAt)
	slog.InfoContext(ctx, "proxy sign-in", slog.String("ip", ip), slog.String("user_id", logged.User.ID))
	return &logged.User, logged.SessionID
}

// DeviceCookie marks a browser where an account has signed in, which exempts that browser's
// logins from the account-wide cap on failed attempts.
const DeviceCookie = "valmin_device"

// SetDeviceCookie writes the trusted-device cookie, kept for maxAge. It is sent only to the
// login route.
func SetDeviceCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: DeviceCookie, Value: value, Path: "/api/v1/auth/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(maxAge.Seconds()),
	})
}
