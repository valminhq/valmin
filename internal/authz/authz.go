// Package authz holds the one authorization function and the closed set of actions it
// decides over.
//
// Specification: 09 §3, 09 §4. Authorization is never middleware (ADR-037): every handler
// calls Can in its own body, because route-pattern authorization fails open — a route
// added later that matches no pattern is unprotected and nothing reports it.
package authz

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/store"
)

// Grants is what Can needs from the database. Defined here rather than in store so the
// consumer owns the interface (06 §4), and so a test can decide without one.
type Grants interface {
	GrantFor(ctx context.Context, userID, instanceID string) (*store.Grant, error)
	GrantedInstances(ctx context.Context, userID string) ([]string, error)
}

// Authz answers every authorization question in the panel.
type Authz struct {
	grants Grants
}

func New(g Grants) *Authz { return &Authz{grants: g} }

// Can reports whether u may perform act on instanceID. An empty instanceID asks about a
// global action.
//
// admin is true always. A member gets the base role of their live grant plus the extras
// toggled on it, and nothing without a grant. Every denial is logged with user, action and
// instance, because denials are a signal (09 §4).
//
// It fails closed: a user that is nil or disabled, a lookup that errors, an instance the
// caller holds nothing on — all false.
func (a *Authz) Can(ctx context.Context, u *store.User, act Action, instanceID string) bool {
	allowed, reason := a.decide(ctx, u, act, instanceID)
	if !allowed {
		userID, role := "", ""
		if u != nil {
			userID, role = u.ID, string(u.Role)
		}
		slog.InfoContext(ctx, "authorization denied",
			slog.String("user_id", userID),
			slog.String("role", role),
			slog.String("action", act.name),
			slog.String("instance_id", instanceID),
			slog.String("reason", reason))
	}
	return allowed
}

// decide is Can without the logging, so the reason can be reported once by its caller
// rather than at each return.
func (a *Authz) decide(
	ctx context.Context, u *store.User, act Action, instanceID string,
) (allowed bool, reason string) {
	switch {
	case u == nil:
		return false, "no authenticated user"
	case u.Disabled:
		return false, "user is disabled"
	case u.Role == store.RoleAdmin:
		return true, ""
	}

	// 09 §3.3: admin-only, globally, with no per-instance override — so this is settled
	// before any grant is read, and a perms row naming one cannot change the answer.
	if neverGrantableSet[act] {
		return false, "action is never grantable to a member"
	}
	if instanceID == "" {
		return false, "member holds no global capability"
	}

	grant, err := a.grants.GrantFor(ctx, u.ID, instanceID)
	if err != nil {
		slog.ErrorContext(ctx, "grant lookup failed", slog.Any("error", err),
			slog.String("user_id", u.ID), slog.String("instance_id", instanceID))
		return false, "grant lookup failed"
	}
	if grant == nil {
		return false, "no live grant on this instance"
	}
	if !granted(grant)[act] {
		return false, "grant does not carry this action"
	}
	return true, ""
}

// Allowed returns the actions u holds on instanceID, sorted, for the allowed_actions
// payload of 09 §4.2. The SPA renders from this list and never from a role name (F3).
func (a *Authz) Allowed(ctx context.Context, u *store.User, instanceID string) ([]Action, error) {
	if u == nil || u.Disabled {
		return []Action{}, nil
	}
	if u.Role == store.RoleAdmin {
		return sorted(everything()), nil
	}

	grant, err := a.grants.GrantFor(ctx, u.ID, instanceID)
	if err != nil {
		return nil, fmt.Errorf("allowed actions for user %s on instance %s: %w", u.ID, instanceID, err)
	}
	if grant == nil {
		return []Action{}, nil
	}
	return sorted(granted(grant)), nil
}

// VisibleInstances reports which instances u may see. all is true for an admin, for whom
// there is no filter to apply; otherwise ids is exactly the set with a live grant, and an
// empty slice means an empty dashboard (09 §1).
func (a *Authz) VisibleInstances(ctx context.Context, u *store.User) (ids []string, all bool, err error) {
	if u == nil || u.Disabled {
		return []string{}, false, nil
	}
	if u.Role == store.RoleAdmin {
		return nil, true, nil
	}
	granted, err := a.grants.GrantedInstances(ctx, u.ID)
	if err != nil {
		return nil, false, fmt.Errorf("visible instances for user %s: %w", u.ID, err)
	}
	if granted == nil {
		granted = []string{}
	}
	return granted, false, nil
}

// granted expands a grant into its action set: the base role, plus the extras, minus
// anything 09 §3.3 keeps admin-only.
func granted(g *store.Grant) map[Action]bool {
	out := make(map[Action]bool, len(roleActions["operator"])+len(g.Perms))
	for act := range roleActions[string(g.Role)] {
		out[act] = true
	}

	for _, name := range g.Perms {
		act, ok := byName[name]
		if !ok {
			// A capability nobody can spell is a grant that silently does nothing, which
			// is the failure shape to report rather than swallow.
			slog.Warn("grant names an unknown capability", slog.String("capability", name))
			continue
		}
		if neverGrantableSet[act] {
			slog.Warn("grant names a never-grantable capability; ignoring",
				slog.String("capability", name))
			continue
		}
		out[act] = true
	}

	// 09 §3.2: config.raw implies config.edit. Raw text editing is strictly the larger
	// power, so holding it without the form is a distinction with no meaning.
	if out[ConfigRaw] {
		out[ConfigEdit] = true
	}
	return out
}

func everything() map[Action]bool {
	out := make(map[Action]bool, len(byName))
	for _, act := range byName {
		out[act] = true
	}
	return out
}

func sorted(m map[Action]bool) []Action {
	out := make([]Action, 0, len(m))
	for act := range m {
		out = append(out, act)
	}
	slices.SortFunc(out, func(x, y Action) int { return strings.Compare(x.name, y.name) })
	return out
}
