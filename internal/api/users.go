package api

import (
	"errors"
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// Users serves the admin-only user-management API 09 §7 puts at M1 ("Admin-only API — no
// UI"). users.manage is on 09 §3.3's never-grantable list, so Can is checked with an empty
// instanceID — a global action — on every route here.
type Users struct {
	DB       *store.DB
	Sessions *auth.Sessions
	Authz    *authz.Authz
}

func (u *Users) Routes(rt *Router) {
	rt.Handle("GET /api/v1/users", http.HandlerFunc(u.list))
	rt.Handle("POST /api/v1/users", http.HandlerFunc(u.create))
	rt.Handle("PATCH /api/v1/users/{id}", http.HandlerFunc(u.update))
	rt.Handle("DELETE /api/v1/users/{id}", http.HandlerFunc(u.delete))
	rt.Handle("POST /api/v1/users/{id}/password/reset", http.HandlerFunc(u.resetPassword))
}

func (u *Users) list(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !u.Authz.Can(r.Context(), caller, authz.UsersManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	users, err := u.DB.ListUsers(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(users, nil))
}

type createUserRequest struct {
	Username string     `json:"username"`
	Password string     `json:"password"`
	Role     store.Role `json:"role"`
}

// create is 04 §3's `POST /users {username, password, role}` — the admin supplies the
// password directly, per that endpoint's own listed body; 09 §5's "generated password" is
// the create-wizard's UX choice (prefill a generated value into this same field), not a
// second API shape. Recorded rather than silently picked: see the M1 plan's write-back.
func (u *Users) create(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !u.Authz.Can(r.Context(), caller, authz.UsersManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	var body createUserRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := validateCredentials(body.Username, body.Password); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var v apierr.Validation
	if body.Role != store.RoleAdmin && body.Role != store.RoleMember {
		v.Add("role", apierr.FieldInvalid, "role must be admin or member.")
	}
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	params, err := auth.LoadArgon2Params(r.Context(), u.DB)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	hash, err := auth.HashPassword(body.Password, params)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	id := store.NewID()
	now := time.Now()
	if err := u.DB.CreateUser(r.Context(), id, body.Username, hash, body.Role, now); err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "username"))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, store.User{ID: id, Username: body.Username, Role: body.Role, CreatedAt: now})
}

type updateUserRequest struct {
	Role     *store.Role `json:"role"`
	Disabled *bool       `json:"disabled"`
}

// update is PATCH /users/{id}: absent means unchanged (11 §1.1), so both fields are read
// from the current row before the one unambiguous write the store layer offers.
func (u *Users) update(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !u.Authz.Can(r.Context(), caller, authz.UsersManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	id := r.PathValue("id")

	var body updateUserRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if body.Role != nil && *body.Role != store.RoleAdmin && *body.Role != store.RoleMember {
		var v apierr.Validation
		v.Add("role", apierr.FieldInvalid, "role must be admin or member.")
		apierr.Write(w, r, v.Err())
		return
	}

	current, err := u.DB.UserByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if current == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	role, disabled := current.Role, current.Disabled
	if body.Role != nil {
		role = *body.Role
	}
	if body.Disabled != nil {
		disabled = *body.Disabled
	}

	if err := u.DB.UpdateUserRoleAndDisabled(r.Context(), id, role, disabled); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	// `↯` Role change and disabling both reach live connections by revoking every
	// session on the account (10 §4.1) — not only future requests, but a socket already
	// open under the old role.
	if disabled || role != current.Role {
		if err := u.Sessions.RevokeAll(r.Context(), id); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
	}

	JSON(w, r, http.StatusOK, store.User{
		ID: id, Username: current.Username, Role: role, Disabled: disabled,
		CreatedAt: current.CreatedAt, LastLoginAt: current.LastLoginAt,
	})
}

func (u *Users) delete(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !u.Authz.Can(r.Context(), caller, authz.UsersManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	id := r.PathValue("id")

	if err := u.DB.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type resetPasswordResponse struct {
	Password string `json:"password"`
}

// resetPassword is 09 §5's admin-issued reset: no SMTP anywhere, so the new password is
// generated here and shown once, the same "shown once" discipline as an invite token.
func (u *Users) resetPassword(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !u.Authz.Can(r.Context(), caller, authz.UsersManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	id := r.PathValue("id")

	password := auth.RandomPassword()
	if err := u.Sessions.SetPassword(r.Context(), id, password); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, resetPasswordResponse{Password: password})
}
