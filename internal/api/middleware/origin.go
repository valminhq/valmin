package middleware

import (
	"net/http"
	"net/url"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// Origin enforces same-origin per 11 §6.1. It sits above authentication so a cross-site
// request never reaches a session cookie.
//
// An absent Origin and an absent Sec-Fetch-Site are allowed: that is curl, and curl cannot
// be a CSRF vector. The WebSocket upgrade inverts this and requires a matching Origin,
// because browsers always send one on an upgrade (11 §6.3).
func Origin(external *url.URL) Layer {
	want := ""
	if external != nil {
		want = external.Scheme + "://" + external.Host
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reason := crossSite(r, want); reason != nil {
				apierr.Write(w, r, apierr.New(apierr.OriginRejected).Wrap(reason))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// crossSite returns why the request declares itself as coming from somewhere other than
// this panel, or nil when it does not.
func crossSite(r *http.Request, want string) error {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "cross-site", "same-site":
		return &siteError{header: "Sec-Fetch-Site", got: site, want: "same-origin"}
	}
	if got := r.Header.Get("Origin"); got != "" && got != want {
		return &siteError{header: "Origin", got: got, want: want}
	}
	return nil
}

// siteError is the cause behind an origin_rejected. It names the offending header for the
// log; the caller only ever sees the generic message (D10).
type siteError struct {
	header string
	got    string
	want   string
}

func (e *siteError) Error() string {
	return e.header + " is " + e.got + ", want " + e.want
}
