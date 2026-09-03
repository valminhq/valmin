package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// Permissions serves the two endpoints that tell the SPA what it may render. Client-side
// hiding is cosmetic; the server checks every request regardless (09 §4.2).
type Permissions struct {
	Authz *authz.Authz
	DB    *store.DB
}

// Routes registers the permission endpoints behind the middleware chain.
func (p *Permissions) Routes(rt *Router) {
	rt.Handle("GET /api/v1/me/permissions", http.HandlerFunc(p.mine))
	rt.Handle("GET /api/v1/instances/{id}/capabilities", http.HandlerFunc(p.capabilities))
}

// caller returns the authenticated user, or writes 401 and reports false. Authentication
// is the chain's job; a handler still checks, because a route reachable without one is a
// route that answers for nobody.
func caller(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u := middleware.UserFrom(r.Context())
	if u == nil {
		apierr.Write(w, r, apierr.New(apierr.Unauthenticated))
		return nil, false
	}
	return u, true
}

// instancePermissions is one entry of the /me/permissions payload.
type instancePermissions struct {
	InstanceID     string         `json:"instance_id"`
	AllowedActions []authz.Action `json:"allowed_actions"`
}

type myPermissions struct {
	UserID string `json:"user_id"`
	// Role is reported so an operator can see it, never so the SPA can branch on it (F3).
	Role store.Role `json:"role"`
	// AllowedActions is the caller's *global* capabilities — 09 §3.3's never-grantable set
	// for an admin, and empty for everyone else.
	//
	// Added for F3, which is otherwise unsatisfiable. 09 §4.2 gives the SPA per-instance
	// actions, and a create button belongs to no instance — so without this the frontend has
	// only `role` to branch on, which is exactly what F3 forbids. `Allowed(u, "")` already
	// answers it: a member holds no global capability, by the same rule Can enforces.
	AllowedActions []authz.Action        `json:"allowed_actions"`
	Instances      []instancePermissions `json:"instances"`
}

// mine is GET /me/permissions (04 §3): the caller's global role and what they may do on
// each instance they can see.
//
// There is no Can call here and 09 §3 has no action for one: the resource is the caller
// themselves, so authentication is the whole check. What the answer *contains* is still
// authorized — VisibleInstances and Allowed are the same seam every other handler uses, so
// a member sees their own grants and nothing else.
func (p *Permissions) mine(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}

	ids, all, err := p.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if all {
		if ids, err = p.DB.AllInstanceIDs(r.Context()); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
	}

	global, err := p.Authz.Allowed(r.Context(), u, "")
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	out := myPermissions{
		UserID: u.ID, Role: u.Role, AllowedActions: global,
		Instances: make([]instancePermissions, 0, len(ids)),
	}
	for _, id := range ids {
		actions, err := p.Authz.Allowed(r.Context(), u, id)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		out.Instances = append(out.Instances, instancePermissions{InstanceID: id, AllowedActions: actions})
	}
	JSON(w, r, http.StatusOK, out)
}

type capabilities struct {
	// CommandChannel resolves to "none" on this build: the server was measured by strace
	// not to read stdin (E3). Detected stays false until the capability probe lands.
	CommandChannel  string         `json:"command_channel"`
	Detected        bool           `json:"detected"`
	AllowedCommands []string       `json:"allowed_commands"`
	AllowedActions  []authz.Action `json:"allowed_actions"`
}

// capabilities is GET /instances/{id}/capabilities (09 §4.2, 07 §5).
func (p *Permissions) capabilities(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	// An instance the caller cannot see does not exist. A 403 here is an existence
	// oracle: iterate ids and map every world on the panel, including the names of ones
	// the caller was deliberately not given (D2, ADR-038).
	if !p.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	exists, err := p.DB.InstanceExists(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if !exists {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	actions, err := p.Authz.Allowed(r.Context(), u, id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, capabilities{
		CommandChannel:  "none",
		Detected:        false,
		AllowedCommands: []string{},
		AllowedActions:  actions,
	})
}
