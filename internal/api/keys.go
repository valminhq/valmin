package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Keys is POST /admin/keys/rotate (04 §3, 10 §3.3). panel.settings is on 09 §3.3's
// never-grantable list, so Can is checked with an empty instanceID and a member sees the
// endpoint as absent (ADR-038).
type Keys struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	Keeper *crypto.Keeper
}

func keyRoutes(rt *routeTable, h *Keys) {
	rt.Handle("POST /api/v1/admin/keys/rotate", http.HandlerFunc(h.rotate))
}

func (h *Keys) rotate(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	ip := middleware.ClientIPFrom(r.Context()).String()
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind:        jobs.KindKeyRotate,
		LockKey:     jobs.GlobalLockKey(jobs.KindKeyRotate),
		Payload:     struct{}{},
		RequestedBy: u.ID,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: u.ID, Action: authz.PanelSettings.String(),
				Detail: `{"operation":"key_rotate"}`, IP: ip,
			}); err != nil {
				return fmt.Errorf("audit key rotation: %w", err)
			}
			return nil
		},
	}, (&crypto.Rotator{DB: h.DB, Keeper: h.Keeper}).Run)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
