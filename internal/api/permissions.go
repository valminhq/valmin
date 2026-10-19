package api

import (
	"context"
	"fmt"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// Permissions serves the two endpoints that tell the SPA what it may render. Client-side
// hiding is cosmetic; the server checks every request regardless (09 §4.2).
type Permissions struct {
	Authz    *authz.Authz
	DB       *store.DB
	Commands *command.Manager
}

// Routes registers the permission endpoints behind the middleware chain.
func permissionRoutes(rt *routeTable, p *Permissions) {
	rt.Handle("GET /api/v1/me/permissions", http.HandlerFunc(p.mine))
	rt.Handle("GET /api/v1/instances/{id}/capabilities", http.HandlerFunc(p.capabilities))
}

// caller returns the authenticated user, or writes 401 and reports false. Authentication
// is the chain's job; a handler still checks, because a route reachable without one is a
// route that answers for nobody.
func caller(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u := middleware.UserFrom(r.Context())
	if u == nil {
		apierr.Write(w, r, apierr.New(errcode.Unauthenticated))
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
	// AllowedActions is the caller's global capabilities: 09 §3.3's never-grantable set for an
	// admin, empty for everyone else. Added for F3, since a create button belongs to no
	// instance and 09 §4.2 gives the SPA only per-instance actions otherwise.
	AllowedActions []authz.Action        `json:"allowed_actions"`
	Instances      []instancePermissions `json:"instances"`
}

// mine is GET /me/permissions (04 §3): the caller's global role and what they may do on each
// instance they can see.
//
// There is no Can call here, since the resource is the caller themselves and authentication
// is the whole check. What the answer contains is still authorized: VisibleInstances and
// Allowed are the same seam every other handler uses.
func (p *Permissions) mine(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}

	ids, all, err := p.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if all {
		if ids, err = p.DB.AllInstanceIDs(r.Context()); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
	}

	global, err := p.Authz.Allowed(r.Context(), u, "")
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	out := myPermissions{
		UserID: u.ID, Role: u.Role, AllowedActions: global,
		Instances: make([]instancePermissions, 0, len(ids)),
	}
	for _, id := range ids {
		actions, err := p.Authz.Allowed(r.Context(), u, id)
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		out.Instances = append(out.Instances, instancePermissions{InstanceID: id, AllowedActions: actions})
	}
	JSON(w, r, http.StatusOK, out)
}

type capabilities struct {
	// CommandChannel resolves to "rcon" when the instance has the ValheimRcon mod.
	// Detected reports whether command capability detection completed.
	CommandChannel  string         `json:"command_channel"`
	Detected        bool           `json:"detected"`
	AllowedCommands []string       `json:"allowed_commands"`
	AllowedActions  []authz.Action `json:"allowed_actions"`
	// WorldTools reports which world-maintenance mods the instance has installed.
	WorldTools worldTools `json:"world_tools"`
	// ValminEye is the installed version of the ValminEye plugin, or null without it.
	ValminEye *string `json:"valmin_eye"`
	// Packages names the mod that provides each capability, for a client to offer.
	Packages capabilityPackages `json:"packages"`
}

type capabilityPackages struct {
	RCON         string `json:"rcon"`
	UpgradeWorld string `json:"upgrade_world"`
	FreshWorld   string `json:"fresh_world"`
}

type worldTools struct {
	UpgradeWorld bool `json:"upgrade_world"`
	FreshWorld   bool `json:"fresh_world"`
}

// eyeVersion is the ValminEye version installed on instanceID, or nil without the plugin.
func (p *Permissions) eyeVersion(ctx context.Context, instanceID string) (*string, error) {
	version, installed, err := p.DB.EnabledModVersion(ctx, instanceID, instance.EyePackage)
	if err != nil {
		return nil, fmt.Errorf("read the ValminEye install: %w", err)
	}
	if !installed {
		return nil, nil
	}
	return &version, nil
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	exists, err := p.DB.InstanceExists(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !exists {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}

	actions, err := p.Authz.Allowed(r.Context(), u, id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	channel := "none"
	detected := false
	allowedCommands := []string{}
	inst, err := p.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if p.Commands != nil {
		available, err := p.Commands.Available(r.Context(), inst)
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		detected = true
		// A caller who may send any command gets no list: the list is a restriction.
		if available {
			channel = "rcon"
			if !p.Authz.Can(r.Context(), u, authz.CommandsUnrestricted, id) {
				allowedCommands = append(allowedCommands, command.AllowedCommands...)
			}
		}
	}
	var tools worldTools
	_, tools.UpgradeWorld, err = p.DB.EnabledModVersion(r.Context(), id, control.UpgradeWorldPackage)
	if err == nil {
		_, tools.FreshWorld, err = p.DB.EnabledModVersion(r.Context(), id, control.FreshWorldPackage)
	}
	var eye *string
	if err == nil {
		eye, err = p.eyeVersion(r.Context(), id)
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, capabilities{
		CommandChannel:  channel,
		Detected:        detected,
		AllowedCommands: allowedCommands,
		AllowedActions:  actions,
		WorldTools:      tools,
		ValminEye:       eye,
		Packages: capabilityPackages{
			RCON: command.ValheimRCONPackage, UpgradeWorld: control.UpgradeWorldPackage,
			FreshWorld: control.FreshWorldPackage,
		},
	})
}
