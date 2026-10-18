package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

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
	Devices   *auth.DeviceTrust

	setupByIP        *ratelimit.Limiter
	loginByIP        *ratelimit.Limiter
	loginByUsername  *ratelimit.Limiter
	failedByUsername *ratelimit.Limiter
	failedByDevice   *ratelimit.Limiter
	passwordByUserID *ratelimit.Limiter
}

// NewAuth wires the dedicated limiters for the setup, login and password-change routes,
// separately from the chain's general per-IP flood guard.
func NewAuth(
	bootstrap *auth.Bootstrap,
	sessions *auth.Sessions,
	gate *middleware.BootstrapGate,
	keeper *crypto.Keeper,
	devices *auth.DeviceTrust,
) *Auth {
	return &Auth{
		Bootstrap: bootstrap, Sessions: sessions, Gate: gate, Keeper: keeper, Devices: devices,
		setupByIP:        perMinute(setupPerMinute, setupBurst),
		loginByIP:        perMinute(loginPerMinute, loginBurst),
		loginByUsername:  perMinute(loginNamePerMinute, loginNameBurst),
		failedByUsername: perMinute(failedLoginPerMinute, failedLoginBurst),
		failedByDevice:   perMinute(trustedLoginPerMinute, trustedLoginBurst),
		passwordByUserID: perMinute(passwordPerMinute, passwordBurst),
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
	if ok, retry := a.setupByIP.Allow(middleware.ClientKey(r.Context())); !ok {
		middleware.RefuseRateLimited(w, r, retry)
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
	client := middleware.ClientKey(r.Context())
	if ok, retry := a.loginByIP.Allow(client); !ok {
		middleware.RefuseRateLimited(w, r, retry)
		return
	}

	var body loginRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	// The username limiter runs after decoding but before the hash, same as the address
	// one: both must reject before argon2id runs. It is keyed on the name at this address, so
	// a stranger guessing a name cannot spend its owner's attempts.
	if ok, retry := a.loginByUsername.Allow(body.Username + "\x00" + client); !ok {
		middleware.RefuseRateLimited(w, r, retry)
		return
	}
	// Failed logins for an account are capped across every address. A browser where it signed
	// in before is held to its own token's cap instead, so guesses from elsewhere cannot lock
	// the owner out, and a leaked token is still bounded. The request reserves a slot before
	// the hash and gets it back when the password was right.
	limiter, key := a.failedByUsername, body.Username
	if device := a.trustedDevice(r, body.Username); device != "" {
		limiter, key = a.failedByDevice, device
	}
	if ok, retry := limiter.Allow(key); !ok {
		middleware.RefuseRateLimited(w, r, retry)
		return
	}

	if refused := a.finishLogin(w, r, body.Username, body.Password); !refused {
		limiter.Refund(key)
	}
}

// trustedDevice is the request's trusted-browser token for username, or "" when it carries
// none that is current.
func (a *Auth) trustedDevice(r *http.Request, username string) string {
	c, err := r.Cookie(middleware.DeviceCookie)
	if err != nil || !a.Devices.Trusted(r.Context(), username, c.Value) {
		return ""
	}
	return c.Value
}

// finishLogin is shared by login and by the two auto-login paths (setup, invite redemption
// once invites.go calls it) — one place sets the cookie pair and answers the body. It reports
// whether the credentials were refused.
func (a *Auth) finishLogin(w http.ResponseWriter, r *http.Request, username, password string) (refused bool) {
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
			return true
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}

	csrfToken, err := middleware.CSRFToken(a.Keeper, logged.SessionID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	middleware.SetSessionCookie(w, logged.Cookie, logged.AbsoluteExpiresAt)
	middleware.SetCSRFCookie(w, csrfToken)
	if device, err := a.Devices.Token(r.Context(), username); err == nil {
		middleware.SetDeviceCookie(w, device, auth.DeviceTrustAge)
	} else {
		slog.WarnContext(r.Context(), "sign trusted device; this browser stays under the login cap",
			slog.Any("error", err))
	}
	JSON(w, r, http.StatusOK, logged.User)
	return false
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
		middleware.RefuseRateLimited(w, r, retry)
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
