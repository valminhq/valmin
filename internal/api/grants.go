package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

type grantChanges interface {
	GrantChanged(ctx context.Context, userID, instanceID string)
}

// Grants serves the admin-only grant-management API.
type Grants struct {
	DB      *store.DB
	Authz   *authz.Authz
	Changes grantChanges
}

func (g *Grants) Routes(rt *Router) {
	rt.Handle("GET /api/v1/instances/{id}/grants", http.HandlerFunc(g.list))
	rt.Handle("GET /api/v1/instances/{id}/grants/{user_id}", http.HandlerFunc(g.get))
	rt.Handle("PUT /api/v1/instances/{id}/grants/{user_id}", http.HandlerFunc(g.put))
	rt.Handle("DELETE /api/v1/instances/{id}/grants/{user_id}", http.HandlerFunc(g.delete))
}

type grantView struct {
	store.GrantRecord
	ETag string `json:"etag"`
}

// grantPage is 11 §1.1's collection with the daemon-owned vocabulary the editor renders
// from, so a role's actions and an extra's risk copy reach the SPA without it deriving
// either from a role name (F3, 09 §4.2).
type grantPage struct {
	Page[grantView]
	Roles             []authz.GrantRoleOption  `json:"roles"`
	ExtraCapabilities []authz.GrantExtraOption `json:"extra_capabilities"`
}

func grantETag(record *store.GrantRecord) (string, error) {
	revision, err := record.Revision()
	if err != nil {
		return "", fmt.Errorf("calculate grant ETag: %w", err)
	}
	return `"` + revision + `"`, nil
}

func (g *Grants) instanceExists(w http.ResponseWriter, r *http.Request) bool {
	exists, err := g.DB.InstanceExists(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if !exists {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return false
	}
	return true
}

func (g *Grants) list(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !g.Authz.Can(r.Context(), caller, authz.GrantsManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !g.instanceExists(w, r) {
		return
	}
	records, err := g.DB.ListGrantRecords(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	items := make([]grantView, 0, len(records))
	for _, record := range records {
		etag, err := grantETag(&record)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		items = append(items, grantView{GrantRecord: record, ETag: etag})
	}
	JSON(w, r, http.StatusOK, grantPage{
		Page:  NewPage(items, nil),
		Roles: authz.GrantRoleOptions(), ExtraCapabilities: authz.GrantExtraOptions(),
	})
}

func (g *Grants) get(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !g.Authz.Can(r.Context(), caller, authz.GrantsManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !g.instanceExists(w, r) {
		return
	}
	record, err := g.DB.GrantRecordFor(r.Context(), r.PathValue("user_id"), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if record == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	etag, err := grantETag(record)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.Header().Set("ETag", etag)
	JSON(w, r, http.StatusOK, grantView{GrantRecord: *record, ETag: etag})
}

type replaceGrantRequest struct {
	Role  store.GrantRole `json:"role"`
	Perms []string        `json:"perms"`
}

func validateGrant(body *replaceGrantRequest) error {
	var validation apierr.Validation
	if body.Role != store.GrantViewer && body.Role != store.GrantOperator {
		validation.Add("role", apierr.FieldInvalid, "role must be viewer or operator.")
	}
	seen := make(map[string]bool, len(body.Perms))
	clean := make([]string, 0, len(body.Perms))
	for _, name := range body.Perms {
		act, ok := authz.ParseAction(name)
		if !ok || !authz.Grantable(act) {
			validation.Add("perms", apierr.FieldInvalid, fmt.Sprintf("%q is not a grantable capability.", name))
			continue
		}
		if !seen[name] {
			seen[name] = true
			clean = append(clean, name)
		}
	}
	slices.Sort(clean)
	body.Perms = clean
	if err := validation.Err(); err != nil {
		return fmt.Errorf("validate grant: %w", err)
	}
	return nil
}

func grantCondition(r *http.Request) (store.GrantCondition, error) {
	match := r.Header.Get("If-Match")
	none := r.Header.Get("If-None-Match")
	if match != "" && none != "" {
		return store.GrantCondition{}, errors.New("If-Match and If-None-Match cannot be combined")
	}
	if none != "" {
		if none != "*" {
			return store.GrantCondition{}, errors.New("If-None-Match must be *")
		}
		return store.GrantCondition{CreateOnly: true}, nil
	}
	if len(match) < 2 || match[0] != '"' || match[len(match)-1] != '"' {
		return store.GrantCondition{}, errors.New("If-Match must contain a grant ETag")
	}
	return store.GrantCondition{Revision: match[1 : len(match)-1]}, nil
}

func auditDetail(userID string, body *replaceGrantRequest) (string, error) {
	b, err := json.Marshal(struct {
		UserID string          `json:"user_id"`
		Role   store.GrantRole `json:"role"`
		Perms  []string        `json:"perms"`
	}{UserID: userID, Role: body.Role, Perms: body.Perms})
	if err != nil {
		return "", fmt.Errorf("encode grant audit detail: %w", err)
	}
	return string(b), nil
}

func (g *Grants) put(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !g.Authz.Can(r.Context(), caller, authz.GrantsManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !g.instanceExists(w, r) {
		return
	}
	condition, err := grantCondition(r)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.InvalidParameter).With("parameter", "If-Match").Wrap(err))
		return
	}
	var body replaceGrantRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := validateGrant(&body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	userID, instanceID := r.PathValue("user_id"), r.PathValue("id")
	detail, err := auditDetail(userID, &body)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	record, created, err := g.DB.ReplaceGrant(r.Context(), userID, instanceID, body.Role, body.Perms,
		condition, &store.AuditEntry{
			UserID: caller.ID, InstanceID: instanceID,
			Action: "instances.grants.write", Detail: detail, IP: middleware.ClientIPFrom(r.Context()).String(),
		})
	if err != nil {
		g.writeMutationError(w, r, err)
		return
	}
	etag, err := grantETag(record)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if g.Changes != nil {
		g.Changes.GrantChanged(r.Context(), userID, instanceID)
	}
	w.Header().Set("ETag", etag)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	JSON(w, r, status, grantView{GrantRecord: *record, ETag: etag})
}

func (g *Grants) delete(w http.ResponseWriter, r *http.Request) {
	caller := middleware.UserFrom(r.Context())
	if !g.Authz.Can(r.Context(), caller, authz.GrantsManage, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !g.instanceExists(w, r) {
		return
	}
	condition, err := grantCondition(r)
	if err == nil && condition.CreateOnly {
		err = errors.New("If-Match is required")
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.InvalidParameter).With("parameter", "If-Match").Wrap(err))
		return
	}
	userID, instanceID := r.PathValue("user_id"), r.PathValue("id")
	detail, err := json.Marshal(map[string]string{"user_id": userID})
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	err = g.DB.DeleteGrant(r.Context(), userID, instanceID, condition.Revision, &store.AuditEntry{
		UserID: caller.ID, InstanceID: instanceID, Action: "instances.grants.delete",
		Detail: string(detail), IP: middleware.ClientIPFrom(r.Context()).String(),
	})
	if err != nil {
		g.writeMutationError(w, r, err)
		return
	}
	if g.Changes != nil {
		g.Changes.GrantChanged(r.Context(), userID, instanceID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Grants) writeMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrGrantPrecondition):
		apierr.Write(w, r, apierr.New(apierr.StaleWrite))
	case errors.Is(err, store.ErrGrantNotFound), errors.Is(err, store.ErrUserNotFound),
		errors.Is(err, store.ErrInstanceNotFound):
		apierr.Write(w, r, apierr.New(apierr.NotFound))
	default:
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
	}
}
