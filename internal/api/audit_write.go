package api

import (
	"context"
	"encoding/json"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/store"
)

// clientIP is the caller's address for an audit entry, empty when the context carries none.
func clientIP(ctx context.Context) string {
	addr := middleware.ClientIPFrom(ctx)
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// detailJSON encodes an entry's structured detail. The values are plain maps and slices, so
// encoding cannot fail in practice; an empty detail is preferable to losing the entry.
func detailJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// jobAudit builds the entry a job Spec carries. A job nobody requested (userID empty: the
// scheduler, crash recovery) is not a privileged action and gets none.
func jobAudit(ctx context.Context, userID, instanceID, action string, detail any) *store.AuditEntry {
	if userID == "" {
		return nil
	}
	return &store.AuditEntry{
		UserID: userID, InstanceID: instanceID, Action: action,
		Detail: detailJSON(detail), IP: clientIP(ctx),
	}
}

// change is one field of a settings diff, as recorded in audit detail. Secret fields carry no
// values: only the fact that they changed.
type change struct {
	Field  string `json:"field"`
	From   any    `json:"from,omitempty"`
	To     any    `json:"to,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}
