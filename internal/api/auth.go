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
	"github.com/valminhq/valmin/internal/store"
)

// minPasswordLength is a policy pick, not a measurement — 03 §1.3's five-character floor
// is the game server's own password, a different secret with different rules. A panel
// account is the credential 02 §6 calls host-root-equivalent, so it gets a higher floor
// and, deliberately, no other complexity rule: argon2id and the login rate limiter are the
// real defenses, and a mandated-symbol rule is exactly the boring-mechanism-violating
// theater 01 §6 argues against.
const minPasswordLength = 8

// Auth serves 10 §6's bootstrap and 10 §4.1's login/logout/me. None of its handlers call
// Can(): the bootstrap endpoint has no caller yet to authorize, and the other three act on
// the caller's own session, which 09 §3 has no action for — the same precedent
// permissions.go's /me/permissions sets.
type Auth struct {
	Bootstrap *auth.Bootstrap
	Sessions  *auth.Sessions
	Gate      *middleware.BootstrapGate
	Keeper    *crypto.Keeper

	setupByIP       *middleware.Limiter
	loginByIP       *middleware.Limiter
	loginByUsername *middleware.Limiter
}

// NewAuth wires the two dedicated limiters 11 §7's table names for these routes,
// separately from the chain's general per-IP flood guard.
func NewAuth(
	bootstrap *auth.Bootstrap,
	sessions *auth.Sessions,
	gate *middleware.BootstrapGate,
	keeper *crypto.Keeper,
) *Auth {
	return &Auth{
		Bootstrap: bootstrap, Sessions: sessions, Gate: gate, Keeper: keeper,
		setupByIP:       middleware.NewLimiter(5, time.Minute, 5),
		loginByIP:       middleware.NewLimiter(10, time.Minute, 10),
		loginByUsername: middleware.NewLimiter(5, time.Minute, 5),
	}
}

func (a *Auth) Routes(rt *Router) {
	rt.Handle("POST /api/v1/setup", http.HandlerFunc(a.setup))
	rt.Handle("POST /api/v1/auth/login", http.HandlerFunc(a.login))
	rt.Handle("POST /api/v1/auth/logout", http.HandlerFunc(a.logout))
	rt.Handle("GET /api/v1/auth/me", http.HandlerFunc(a.me))
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
		apierr.Write(w, r, apierr.New(apierr.RateLimited))
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
		apierr.Write(w, r, apierr.New(apierr.SetupConsumed))
	case errors.Is(err, auth.ErrSetupTokenInvalid):
		var v apierr.Validation
		v.Add("token", apierr.FieldInvalid, "That token is invalid or expired.")
		apierr.Write(w, r, v.Err())
	case errors.Is(err, store.ErrUsernameTaken):
		apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "username"))
	default:
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(apierr.RateLimited))
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
		apierr.Write(w, r, apierr.New(apierr.RateLimited))
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
			apierr.Write(w, r, apierr.New(apierr.InvalidCredentials))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	csrfToken, err := middleware.CSRFToken(a.Keeper, logged.SessionID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	middleware.SetSessionCookie(w, logged.Cookie, logged.AbsoluteExpiresAt)
	middleware.SetCSRFCookie(w, csrfToken)
	JSON(w, r, http.StatusOK, logged.User)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if sessionID := middleware.SessionIDFrom(r.Context()); sessionID != "" {
		if err := a.Sessions.Logout(r.Context(), sessionID); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(apierr.Unauthenticated))
		return
	}
	JSON(w, r, http.StatusOK, u)
}

// validateCredentials is 04 §3's shared shape for setup, direct user creation and invite
// redemption: a username and a password, both required, the password above the floor.
func validateCredentials(username, password string) error {
	var v apierr.Validation
	if username == "" {
		v.Add("username", apierr.FieldRequired, "A username is required.")
	}
	if password == "" {
		v.Add("password", apierr.FieldRequired, "A password is required.")
	} else if len(password) < minPasswordLength {
		v.Add("password", apierr.FieldTooShort, "Password must be at least 8 characters.")
	}
	if err := v.Err(); err != nil {
		return fmt.Errorf("validate credentials: %w", err)
	}
	return nil
}

func writeRetryAfter(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
}
