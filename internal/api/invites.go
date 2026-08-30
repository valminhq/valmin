package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/store"
)

// Invites serves 09 §5. invites.manage is on 09 §3.3's never-grantable list — issuing,
// listing and revoking are admin-only, checked as a global action. Redemption is the one
// unauthenticated route in this file.
type Invites struct {
	DB       *store.DB
	Invites  *auth.Invites
	Sessions *auth.Sessions
	Authz    *authz.Authz
	Keeper   *crypto.Keeper
	// ExternalURL builds the redemption link 04 §3 promises in the issue response.
	ExternalURL string

	redeemByIP *middleware.Limiter
}

func NewInvites(
	db *store.DB,
	invites *auth.Invites,
	sessions *auth.Sessions,
	az *authz.Authz,
	keeper *crypto.Keeper,
	externalURL string,
) *Invites {
	return &Invites{
		DB: db, Invites: invites, Sessions: sessions, Authz: az, Keeper: keeper, ExternalURL: externalURL,
		redeemByIP: middleware.NewLimiter(10, time.Minute, 10),
	}
}

func (i *Invites) Routes(rt *Router) {
	rt.Handle("POST /api/v1/invites", http.HandlerFunc(i.issue))
	rt.Handle("GET /api/v1/invites", http.HandlerFunc(i.list))
	rt.Handle("DELETE /api/v1/invites/{id}", http.HandlerFunc(i.revoke))
	rt.Handle("POST /api/v1/invites/{token}/redeem", http.HandlerFunc(i.redeem))
}

type issueInviteRequest struct {
	InstanceID *string          `json:"instance_id"`
	GrantRole  *store.GrantRole `json:"grant_role"`
	GrantPerms []string         `json:"grant_perms"`
}

type issuedInviteResponse struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// issue is POST /invites (09 §5, 04 §3): "an invite can only grant what the issuer holds
// — and since only admins issue invites, that is everything. It still must not be possible
// to mint an invite that confers admin" — there is no admin grant concept to confer (09 §2
// gives global roles, not per-instance ones), so that guarantee holds by construction; what
// this validates is 09 §3.3's never-grantable list, so an invite cannot smuggle
// instance.delete or users.manage into someone's perms.
func (i *Invites) issue(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !i.Authz.Can(r.Context(), caller, authz.InvitesManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	var body issueInviteRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := i.validateIssue(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	permsJSON, err := json.Marshal(body.GrantPerms)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	issued, err := i.Invites.Issue(r.Context(), caller.ID, body.InstanceID, body.GrantRole, string(permsJSON))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	JSON(w, r, http.StatusCreated, issuedInviteResponse{
		Token: issued.Code, URL: i.ExternalURL + "/redeem/" + issued.Code, ExpiresAt: issued.ExpiresAt,
	})
}

// validateIssue holds every 422-shaped check on an issueInviteRequest: the instance/role
// pairing, that the named instance exists, the role enum, and that every requested
// capability both parses and is grantable (09 §3.3 — never-grantable has no per-instance
// override, and an invite must not be the loophole that smuggles one in).
func (i *Invites) validateIssue(r *http.Request, body *issueInviteRequest) error {
	var v apierr.Validation
	if (body.InstanceID == nil) != (body.GrantRole == nil) {
		v.Add("instance_id", apierr.FieldInvalid,
			"An instance and a grant role must be given together, or neither.")
	}
	if body.InstanceID != nil {
		exists, err := i.DB.InstanceExists(r.Context(), *body.InstanceID)
		if err != nil {
			return fmt.Errorf("check instance exists: %w", err)
		}
		if !exists {
			v.Add("instance_id", apierr.FieldInvalid, "No such instance.")
		}
	}
	if body.GrantRole != nil && *body.GrantRole != store.GrantViewer && *body.GrantRole != store.GrantOperator {
		v.Add("grant_role", apierr.FieldInvalid, "grant_role must be viewer or operator.")
	}
	for _, name := range body.GrantPerms {
		act, ok := authz.ParseAction(name)
		if !ok || !authz.Grantable(act) {
			v.Add("grant_perms", apierr.FieldInvalid, fmt.Sprintf("%q is not a grantable capability.", name))
			break
		}
	}
	if err := v.Err(); err != nil {
		return fmt.Errorf("validate invite: %w", err)
	}
	return nil
}

func (i *Invites) list(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !i.Authz.Can(r.Context(), caller, authz.InvitesManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	list, err := i.Invites.List(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(list, nil))
}

func (i *Invites) revoke(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !i.Authz.Can(r.Context(), caller, authz.InvitesManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	id := r.PathValue("id")
	inv, err := i.DB.InviteByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inv == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if err := i.Invites.Revoke(r.Context(), id); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type redeemInviteRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// redeem is the one unauthenticated route in this file (09 §5). Every dead reason —
// expired, revoked, already redeemed, never existed — is the same invite_invalid, or the
// endpoint is a token oracle.
func (i *Invites) redeem(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIPFrom(r.Context()).String()
	if ok, retry := i.redeemByIP.Allow(ip); !ok {
		writeRetryAfter(w, retry)
		apierr.Write(w, r, apierr.New(apierr.RateLimited))
		return
	}

	token := r.PathValue("token")
	var body redeemInviteRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := validateCredentials(body.Username, body.Password); err != nil {
		apierr.Write(w, r, err)
		return
	}

	if _, _, err := i.Invites.Redeem(r.Context(), token, body.Username, body.Password); err != nil {
		if errors.Is(err, auth.ErrInviteInvalid) {
			apierr.Write(w, r, apierr.New(apierr.InviteInvalid))
			return
		}
		if errors.Is(err, store.ErrUsernameTaken) {
			apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "username"))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	logged, err := i.Sessions.Login(r.Context(), body.Username, body.Password, ip, r.UserAgent())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	csrfToken, err := middleware.CSRFToken(i.Keeper, logged.SessionID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	middleware.SetSessionCookie(w, logged.Cookie, logged.AbsoluteExpiresAt)
	middleware.SetCSRFCookie(w, csrfToken)
	JSON(w, r, http.StatusOK, logged.User)
}
