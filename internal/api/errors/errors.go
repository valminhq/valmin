// Package errors holds the error envelope and its closed code registry.
//
// Specification: 11 §2.
package errors

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// RequestPath returns a log-safe path with invite credentials removed.
func RequestPath(r *http.Request) string {
	path := r.URL.Path
	const apiPrefix = "/api/v1/invites/"
	if strings.HasPrefix(path, apiPrefix) && strings.HasSuffix(path, "/redeem") {
		return apiPrefix + "{token}/redeem"
	}
	if strings.HasPrefix(path, "/redeem/") {
		return "/redeem/{token}"
	}
	return path
}

// Code names a failure. The registry below is closed (ADR-034): the unexported fields mean
// no other package can mint a Code, so a code that is not in the table does not compile.
// The same values fill job_runs.error_code, which is why some carry no HTTP status.
type Code struct {
	name    string
	status  int
	message string
}

// String returns the wire form, which is also the job_runs.error_code form.
func (c Code) String() string { return c.name }

// Status returns the HTTP status 11 §2.5 pairs with the code, or 0 for a job-only code.
func (c Code) Status() int { return c.status }

// Message returns the sentence the caller sees. Write and the envelope render it for an
// HTTP response; the WebSocket error message of 04 §4 carries the same text on a transport
// that has no envelope of its own.
func (c Code) Message() string { return c.message }

// MarshalJSON renders the code as its wire name.
func (c Code) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(c.name)), nil }

// The registry of 11 §2.5, one var per row: name, status, and the sentence the caller
// sees. Messages are written for the person who hit the error rather than the person who
// wrote it, and are safe to render to any caller (D10).
var (
	Unauthenticated    = Code{"unauthenticated", 401, "Sign in to continue."}
	InvalidCredentials = Code{"invalid_credentials", 401, "The username or password is incorrect."}
	TOTPRequired       = Code{"totp_required", 401, "Enter an authentication code to finish signing in."}
	Forbidden          = Code{"forbidden", 403, "You do not have permission to perform this action."}
	CSRFFailed         = Code{"csrf_failed", 403, "This request could not be verified. Reload the page and try again."}
	OriginRejected     = Code{
		"origin_rejected",
		403,
		"This request was blocked because it came from another site. Open Valmin directly to continue.",
	}
	SetupRequired = Code{"setup_required", 503, "This panel has not been set up yet."}
	SetupConsumed = Code{"setup_consumed", 410, "Setup is already complete. Sign in with an existing account."}
	InviteInvalid = Code{
		"invite_invalid",
		410,
		"This invite is invalid, expired, revoked, or already used. Ask an administrator for a new invite.",
	}
	RateLimited = Code{"rate_limited", 429, "Too many requests. Try again shortly."}

	MalformedJSON    = Code{"malformed_json", 400, "The request body is not valid JSON."}
	InvalidParameter = Code{"invalid_parameter", 400, "A request parameter is not valid."}
	ValidationFailed = Code{"validation_failed", 422, "Some values are invalid. Check the validation details."}
	NotFound         = Code{"not_found", 404, "The requested item could not be found."}
	MethodNotAllowed = Code{"method_not_allowed", 405, "This action is not supported at this address."}
	StaleWrite       = Code{
		"stale_write",
		412,
		"The saved version changed after you loaded it. Review the latest version before saving.",
	}
	PayloadTooLarge      = Code{"payload_too_large", 413, "The upload exceeds the allowed size. Choose a smaller file."}
	UnsupportedMediaType = Code{"unsupported_media_type", 415, "This file or request format is not supported."}

	InvalidState = Code{
		"invalid_state",
		409,
		"This action is unavailable in the server's current state. Check its status before trying again.",
	}
	JobInProgress = Code{
		"job_in_progress",
		409,
		"Another task is running on this server. Wait for it to finish.",
	}
	InstanceMustBeStopped = Code{"instance_must_be_stopped", 409, "Stop the server before performing this action."}
	JobNotCancellable     = Code{"job_not_cancellable", 409, "This job is past the point where it can be cancelled."}
	NameTaken             = Code{"name_taken", 409, "This name is already in use. Choose a different name."}
	PortExhausted         = Code{"port_exhausted", 409, "There is no free port range left on this host."}
	InsufficientDisk      = Code{"insufficient_disk", 409, "There is not enough free disk space."}
	Unsupported           = Code{"unsupported", 409, "This server build does not support that."}
	WorldPairIncomplete   = Code{"world_pair_incomplete", 422, "A world needs both its .db and .fwl file."}
	DependencyUnresolved  = Code{"dependency_unresolved", 409, "A required mod dependency is missing."}
	PackageInvalid        = Code{"package_invalid", 422, "That mod package cannot be installed."}
	ModConflict           = Code{"mod_conflict", 409, "That conflicts with a mod already installed."}
	ContainerMismatch     = Code{
		"container_mismatch",
		409,
		"This container does not match the configuration required for recovery. Check the server settings and container configuration.",
	}
	// BackupUnverifiable is job-only: the quiesce did not confirm the world was saved, or the
	// archive that was written does not verify. Either way no archive is published and no
	// catalogue row is written (02 §4.4, 12 §3.4, B8).
	BackupUnverifiable = Code{"backup_unverifiable", 0, "Valmin could not verify that the backup is complete."}
	Interrupted        = Code{"interrupted", 0, "The panel stopped while this job was running."}
	Timeout            = Code{"timeout", 504, "The request timed out. Check the current status before trying again."}
	Stalled            = Code{"stalled", 0, "This job stopped making progress."}

	Internal    = Code{"internal", 500, "Valmin could not complete the request."}
	Unavailable = Code{"unavailable", 503, "Valmin is temporarily unavailable. Check its status before trying again."}
)

// requestIDKey is the context key the request id travels under. It lives here, in the
// package that has to read it, rather than in middleware — which imports this one, so the
// dependency cannot point the other way.
type requestIDKey struct{}

// WithRequestID attaches the id the middleware minted.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the id, or "" outside the chain.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Error is a failure on its way to the envelope of 11 §2.1. Message and Details are what
// the caller sees; the wrapped cause is what the log gets and the caller never does.
type Error struct {
	Code    Code
	Message string
	Details map[string]any

	cause error
}

// New starts an error from a registry code, carrying that code's default message.
func New(c Code) *Error { return &Error{Code: c, Message: c.message} }

// Msg replaces the code's default message with one written for this call site.
func (e *Error) Msg(s string) *Error {
	e.Message = s
	return e
}

// With adds one of the details keys documented for this code in 11 §2.5.
func (e *Error) With(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 1)
	}
	e.Details[key] = value
	return e
}

// Wrap records the cause. It reaches the log and never the response.
func (e *Error) Wrap(err error) *Error {
	e.cause = err
	return e
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.Code.name
	}
	return e.Code.name + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error { return e.cause }

// As unwraps err to the *Error it carries. A bare error arriving at a handler boundary is
// a bug, and 500 with the chain in the log is how that reads in production.
func As(err error) *Error {
	var e *Error
	if stderrors.As(err, &e) {
		return e
	}
	return New(Internal).Wrap(err)
}

// envelope is the response body of 11 §2.1. Errors are never a bare string.
type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
}

// Write renders err as the envelope of 11 §2.1 and logs the full %w chain under the same
// request id: a generic message goes to the caller, the real cause to the log, since a wrapped
// filesystem path is not for a member with one grant to see (D10).
//
// The request id comes from the context, with the X-Request-Id header as a fallback for a
// writer that never passed through it.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	e := As(err)
	if e.Code.status == 0 {
		// A job-only code, or a zero Code that escaped a struct literal. Neither has an
		// HTTP meaning, and answering with a blank status would hide the bug.
		slog.ErrorContext(r.Context(), "error code has no HTTP status",
			slog.String("code", e.Code.name), slog.String("path", RequestPath(r)))
		e = New(Internal).Wrap(err)
	}

	// The context first, the header only as a fallback: http.TimeoutHandler hands the handler a
	// ResponseWriter with its own empty header map, so reading X-Request-Id off the writer would
	// silently empty request_id on every timeout (D10, 11 §2.1).
	requestID := RequestIDFrom(r.Context())
	if requestID == "" {
		requestID = w.Header().Get("X-Request-Id")
	}

	out := body{
		Code:      e.Code,
		Message:   e.Message,
		Details:   e.Details,
		RequestID: requestID,
	}
	if e.Code == Internal {
		// 11 §2.5: the only 500, and it never carries details.
		out.Details = nil
	}

	log := slog.WarnContext
	if e.Code.status >= 500 {
		log = slog.ErrorContext
	}
	log(r.Context(), "request failed",
		slog.String("code", e.Code.name),
		slog.Int("status", e.Code.status),
		slog.String("method", r.Method),
		slog.String("path", RequestPath(r)),
		slog.String("request_id", out.RequestID),
		slog.Any("error", err))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.Code.status)
	if err := json.NewEncoder(w).Encode(envelope{Error: out}); err != nil {
		slog.ErrorContext(r.Context(), "writing error envelope", slog.Any("error", err))
	}
}
