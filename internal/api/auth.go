package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/ratelimit"
	"github.com/valminhq/valmin/internal/store"
)

// minPasswordLength is a policy pick, not a measurement: a panel account is a
// host-root-equivalent credential (02 §6), distinct from 03 §1.3's game-password floor, and
// gets a higher one with no other complexity rule, argon2id and the rate limiter being the
// real defenses (01 §6).
const minPasswordLength = 8

// Auth serves bootstrap, login, logout, the caller's own record and the caller's own password
// change. None of its handlers call Can(): bootstrap and login have no caller yet to authorize,
// and the rest act on the caller's own session or account, which has no action.
type Auth struct {
	Bootstrap *auth.Bootstrap
	Sessions  *auth.Sessions
	Gate      *middleware.BootstrapGate
	Keeper    *crypto.Keeper

	setupByIP        *ratelimit.Limiter
	loginByIP        *ratelimit.Limiter
	loginByUsername  *ratelimit.Limiter
	passwordByUserID *ratelimit.Limiter
}

// NewAuth wires the dedicated limiters for the setup, login and password-change routes,
// separately from the chain's general per-IP flood guard.
func NewAuth(
	bootstrap *auth.Bootstrap,
	sessions *auth.Sessions,
	gate *middleware.BootstrapGate,
	keeper *crypto.Keeper,
) *Auth {
	return &Auth{
		Bootstrap: bootstrap, Sessions: sessions, Gate: gate, Keeper: keeper,
		setupByIP:        ratelimit.New(5, time.Minute, 5),
		loginByIP:        ratelimit.New(10, time.Minute, 10),
		loginByUsername:  ratelimit.New(5, time.Minute, 5),
		passwordByUserID: ratelimit.New(5, time.Minute, 5),
	}
}

func authRoutes(rt *routeTable, a *Auth) {
	rt.Handle("POST /api/v1/setup", http.HandlerFunc(a.setup))
	rt.Handle("POST /api/v1/auth/login", http.HandlerFunc(a.login))
	rt.Handle("POST /api/v1/auth/logout", http.HandlerFunc(a.logout))
	rt.Handle("GET /api/v1/auth/me", http.HandlerFunc(a.me))
	rt.Handle("POST /api/v1/me/password", http.HandlerFunc(a.changePassword))
}

type setupRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// setup is 10 §6. Success logs the new admin straight in — 04 §3 does not say either way,
// and asking someone to re-enter the password they just typed is the worse reading.
func (a *Auth) setup(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIPFrom(r.Context()).String()
	if ok, retry := a.setupByIP.Allow(ip); !ok {
		writeRetryAfter(w, retry)
		apierr.Write(w, r, apierr.New(errcode.RateLimited))
		return
	}

	var body setupRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := validateCredentials(body.Username, body.Password); err != nil {
		apierr.Write(w, r, err)
		return
	}

	if _, err := a.Bootstrap.Setup(r.Context(), body.Token, body.Username, body.Password); err != nil {
		writeSetupError(w, r, err)
		return
	}
	a.Gate.Complete()

	a.finishLogin(w, r, body.Username, body.Password)
}

// writeSetupError classifies auth.Bootstrap.Setup's sentinels. A bad token is a 422 on
// the token field rather than a new top-level code: ADR-034 closes the registry, and this
// is a per-field problem the same shape as any other.
func writeSetupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrSetupConsumed), errors.Is(err, store.ErrBootstrapConsumed):
		apierr.Write(w, r, apierr.New(errcode.SetupConsumed))
	case errors.Is(err, auth.ErrSetupTokenInvalid):
		var v apierr.Validation
		v.Add("token", apierr.FieldInvalid, "That token is invalid or expired.")
		apierr.Write(w, r, v.Err())
	case errors.Is(err, store.ErrUsernameTaken):
		apierr.Write(w, r, apierr.New(errcode.NameTaken).With("field", "username"))
	default:
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIPFrom(r.Context()).String()
	if ok, retry := a.loginByIP.Allow(ip); !ok {
		writeRetryAfter(w, retry)
		apierr.Write(w, r, apierr.New(errcode.RateLimited))
		return
	}

	var body loginRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	// The username limiter runs after decoding but before the hash, same as the IP
	// one — both must reject before argon2id runs, or a name in a tight loop is a memory
	// amplifier regardless of which key the limiter watches (D12, 11 §7).
	if ok, retry := a.loginByUsername.Allow(body.Username); !ok {
		writeRetryAfter(w, retry)
		apierr.Write(w, r, apierr.New(errcode.RateLimited))
		return
	}

	a.finishLogin(w, r, body.Username, body.Password)
}

// finishLogin is shared by login and by the two auto-login paths (setup, invite redemption
// once invites.go calls it) — one place sets the cookie pair and answers the body.
func (a *Auth) finishLogin(w http.ResponseWriter, r *http.Request, username, password string) {
	logged, err := a.Sessions.Login(
		r.Context(),
		username,
		password,
		middleware.ClientIPFrom(r.Context()).String(),
		r.UserAgent(),
	)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrAccountDisabled) {
			// Identical response for both (11 §2.5): a disabled account must not be
			// distinguishable from a wrong password, or the endpoint becomes an oracle
			// for "this username exists and is disabled".
			apierr.Write(w, r, apierr.New(errcode.InvalidCredentials))
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	csrfToken, err := middleware.CSRFToken(a.Keeper, logged.SessionID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	middleware.SetSessionCookie(w, logged.Cookie, logged.AbsoluteExpiresAt)
	middleware.SetCSRFCookie(w, csrfToken)
	JSON(w, r, http.StatusOK, logged.User)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if sessionID := middleware.SessionIDFrom(r.Context()); sessionID != "" {
		if err := a.Sessions.Logout(r.Context(), sessionID); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
	}
	middleware.ClearSessionCookie(w)
	middleware.ClearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) me(w http.ResponseWriter, r *http.Request) {
	u := middleware.UserFrom(r.Context())
	if u == nil {
		apierr.Write(w, r, apierr.New(errcode.Unauthenticated))
		return
	}
	JSON(w, r, http.StatusOK, u)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword is POST /me/password: the caller replaces their own password after proving the
// current one. Their other sessions end and the one making the request stays valid. The
// per-account limiter bounds guessing of the current password from a stolen session.
func (a *Auth) changePassword(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if caller == nil {
		apierr.Write(w, r, apierr.New(errcode.Unauthenticated))
		return
	}

	var body changePasswordRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var v apierr.Validation
	if body.CurrentPassword == "" {
		v.Add("current_password", apierr.FieldRequired, "Enter your current password.")
	}
	checkPassword(&v, "new_password", body.NewPassword)
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	if ok, retry := a.passwordByUserID.Allow(caller.ID); !ok {
		writeRetryAfter(w, retry)
		apierr.Write(w, r, apierr.New(errcode.RateLimited))
		return
	}

	detail, err := userAuditDetail(map[string]string{auditTargetUser: caller.ID})
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	err = a.Sessions.ChangePassword(
		r.Context(), caller.Username, middleware.SessionIDFrom(r.Context()),
		body.CurrentPassword, body.NewPassword, &store.AuditEntry{
			UserID: caller.ID, Action: "users.password.change", Detail: detail,
			IP: middleware.ClientIPFrom(r.Context()).String(),
		})
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		apierr.Write(w, r, apierr.New(errcode.InvalidCredentials).Msg("The current password is incorrect."))
	case err != nil:
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// validateCredentials is 04 §3's shared shape for setup, direct user creation and invite
// redemption: a username and a password, both required, the password above the floor.
func validateCredentials(username, password string) error {
	var v apierr.Validation
	if username == "" {
		v.Add("username", apierr.FieldRequired, "A username is required.")
	}
	checkPassword(&v, "password", password)
	if err := v.Err(); err != nil {
		return fmt.Errorf("validate credentials: %w", err)
	}
	return nil
}

// checkPassword records against field that the password is missing or shorter than the floor.
func checkPassword(v *apierr.Validation, field, password string) {
	if password == "" {
		v.Add(field, apierr.FieldRequired, "A password is required.")
	} else if len(password) < minPasswordLength {
		v.Add(field, apierr.FieldTooShort, "Password must be at least 8 characters.")
	}
}

func writeRetryAfter(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
}
