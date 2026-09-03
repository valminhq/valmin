package middleware

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/crypto"
)

// CSRFCookie is the double-submit cookie of 11 §6.2. Unlike the session cookie it is
// readable by JS, because the SPA has to echo it back in X-CSRF-Token.
const CSRFCookie = "valmin_csrf"

// CSRFHeader is where the SPA returns the cookie's value.
const CSRFHeader = "X-CSRF-Token"

// CSRFToken derives the token for a session: HMAC of the session id under the csrf subkey
// (10 §3.2). Deriving rather than storing means there is no server-side CSRF table to keep
// in step with the session table, and the token is bound to exactly one session.
func CSRFToken(k *crypto.Keeper, sessionID string) (string, error) {
	mac, err := k.MAC(crypto.PurposeCSRF, []byte(sessionID))
	if err != nil {
		return "", fmt.Errorf("derive csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(mac), nil
}

// SetCSRFCookie writes the double-submit cookie alongside a new session.
func SetCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: false, // the SPA must read it to echo it back
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCSRFCookie expires the cookie on logout and on session rotation.
func ClearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CSRFCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// CSRF verifies the double-submit token on state-changing methods (11 §5.1 row 10). It
// sits below session authentication because the token is bound to the session, and it is
// the third of three layers with different failure modes: SameSite=Strict fails on browser
// quirks, the origin check fails on a misconfigured proxy, and this one fails on neither.
//
// A request with no session has nothing to forge against and is left to the origin check
// and the rate limiter above.
//
// `↯` On an authenticated **non**-state-changing request the cookie is re-asserted rather
// than checked. Without that, the two cookies of 11 §6.2 have different lifetimes — the
// session cookie carries the session's absolute expiry and survives a browser restart, the
// CSRF cookie carries none and does not — so reopening the browser leaves a valid session
// with no token. Every state-changing request then answers 403, **including logout and
// login**, and the panel has no route back: the operator cannot end the session that is
// causing the failure, and re-authenticating is itself a POST. Observed 3 Sep 2026, on a
// session opened at 01:58 and used again at 09:08.
//
// Re-asserting is free of security weight. The value is derived from the session id, so it
// is not a secret the response is leaking: an attacker who could read this Set-Cookie could
// already read the session cookie it is derived from. The comparison below is still against
// the re-derived value and never against the cookie, which is the property 11 §6.2 marks —
// the cookie remains delivery only, now on every authenticated read rather than only at
// session creation. Session expiry stays server-side where 10 §4.1 enforces it.
func CSRF(k *crypto.Keeper) Layer {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session := SessionIDFrom(r.Context())
			if session == "" {
				next.ServeHTTP(w, r)
				return
			}

			want, err := CSRFToken(k, session)
			if err != nil {
				apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
				return
			}
			if !stateChanging(r.Method) {
				SetCSRFCookie(w, want)
				next.ServeHTTP(w, r)
				return
			}

			got := r.Header.Get(CSRFHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				apierr.Write(w, r, apierr.New(apierr.CSRFFailed).
					Wrap(fmt.Errorf("csrf token mismatch on %s %s", r.Method, r.URL.Path)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
