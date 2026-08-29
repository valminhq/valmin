package api

import (
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// JSON writes v as the response body. Nothing else in the package writes a body directly,
// so the content type and the no-store header cannot drift apart.
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already on the wire; all that is left is to say so in the log.
		slog.ErrorContext(r.Context(), "writing response body", slog.Any("error", err))
	}
}

// Accepted is the 202 of 11 §3. Reaching it means the job row exists and its lock is held:
// a job that could not take its lock is 409 job_in_progress, never a 202 for work silently
// queued behind another (ADR-030).
func Accepted(w http.ResponseWriter, r *http.Request, jobID string, stub any) {
	w.Header().Set("Location", "/api/v1/jobs/"+jobID)
	JSON(w, r, http.StatusAccepted, stub)
}

// Decode reads a JSON request body into v. Unknown fields are rejected rather than
// ignored: a mistyped field that silently does nothing is the failure shape this project
// designs against.
func Decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			return apierr.New(apierr.UnsupportedMediaType).With("content_type", ct)
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if stderrors.As(err, &tooLarge) {
			return apierr.New(apierr.PayloadTooLarge).With("limit_bytes", tooLarge.Limit).Wrap(err)
		}
		return apierr.New(apierr.MalformedJSON).Wrap(err)
	}
	return nil
}

// Page is the collection shape of 11 §1.1: always an object, never a bare array. Total is
// nil unless counting is cheap, and next_cursor nil is the end — there is no has_more as
// well, because one signal cannot disagree with itself.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	Total      *int    `json:"total"`
}

// NewPage normalises the empty case, so an empty collection is [] rather than null.
func NewPage[T any](items []T, next *string) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{Items: items, NextCursor: next}
}

// Cursor is the keyset position of ADR-035: the sort key plus the id that breaks ties, so
// a page boundary stays stable under concurrent inserts. It is opaque to clients, who must
// neither construct nor parse one.
type Cursor struct {
	SortKey string `json:"k"`
	ID      string `json:"i"`
}

// Encode renders the cursor for next_cursor.
func (c Cursor) Encode() string {
	raw, err := json.Marshal(c)
	if err != nil {
		// Two strings cannot fail to marshal.
		panic("encoding cursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ParseCursor reads the cursor query parameter. An absent cursor is the first page.
func ParseCursor(r *http.Request) (Cursor, bool, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return Cursor{}, false, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, false, apierr.New(apierr.InvalidParameter).With("parameter", "cursor").Wrap(err)
	}
	var c Cursor
	if err := json.Unmarshal(decoded, &c); err != nil {
		return Cursor{}, false, apierr.New(apierr.InvalidParameter).With("parameter", "cursor").Wrap(err)
	}
	return c, true, nil
}

// Pagination bounds from 11 §4.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// ParseLimit reads the limit query parameter. A value over the cap is clamped rather than
// rejected: a client asking for too much wants as much as it can have.
func ParseLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, apierr.New(apierr.InvalidParameter).
			With("parameter", "limit").
			Wrap(fmt.Errorf("limit %q is not a positive integer", raw))
	}
	return min(n, MaxLimit), nil
}
