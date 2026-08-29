package errors

import (
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net/http"
	"strconv"
)

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

// MarshalJSON renders the code as its wire name.
func (c Code) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(c.name)), nil }

// The registry of 11 §2.5, one var per row: name, status, and the sentence the caller
// sees. Messages are written for the person who hit the error rather than the person who
// wrote it, and are safe to render to any caller (D10).
var (
	Unauthenticated    = Code{"unauthenticated", 401, "You are not signed in."}
	InvalidCredentials = Code{"invalid_credentials", 401, "That username or password is not right."}
	TOTPRequired       = Code{"totp_required", 401, "This account needs a second factor."}
	Forbidden          = Code{"forbidden", 403, "You are not allowed to do that."}
	CSRFFailed         = Code{"csrf_failed", 403, "This request could not be verified. Reload the page and try again."}
	OriginRejected     = Code{"origin_rejected", 403, "This request came from another site."}
	SetupRequired      = Code{"setup_required", 503, "This panel has not been set up yet."}
	SetupConsumed      = Code{"setup_consumed", 410, "This panel already has an administrator."}
	InviteInvalid      = Code{"invite_invalid", 410, "That invite cannot be used."}
	RateLimited        = Code{"rate_limited", 429, "Too many requests. Try again shortly."}

	MalformedJSON        = Code{"malformed_json", 400, "The request body is not valid JSON."}
	InvalidParameter     = Code{"invalid_parameter", 400, "A request parameter is not valid."}
	ValidationFailed     = Code{"validation_failed", 422, "Some fields need fixing."}
	NotFound             = Code{"not_found", 404, "That does not exist."}
	MethodNotAllowed     = Code{"method_not_allowed", 405, "That method is not allowed here."}
	StaleWrite           = Code{"stale_write", 412, "Someone else changed this since you loaded it."}
	PayloadTooLarge      = Code{"payload_too_large", 413, "That upload is larger than this endpoint accepts."}
	UnsupportedMediaType = Code{"unsupported_media_type", 415, "That content type is not accepted here."}

	InvalidState          = Code{"invalid_state", 409, "The server is not in a state where that is possible."}
	JobInProgress         = Code{"job_in_progress", 409, "Something else is already running on this server."}
	InstanceMustBeStopped = Code{"instance_must_be_stopped", 409, "The server must be stopped first."}
	JobNotCancellable     = Code{"job_not_cancellable", 409, "This job is past the point where it can be cancelled."}
	NameTaken             = Code{"name_taken", 409, "That name is already in use."}
	PortExhausted         = Code{"port_exhausted", 409, "There is no free port range left on this host."}
	InsufficientDisk      = Code{"insufficient_disk", 409, "There is not enough free disk space."}
	Unsupported           = Code{"unsupported", 409, "This server build does not support that."}
	WorldPairIncomplete   = Code{"world_pair_incomplete", 422, "A world needs both its .db and .fwl file."}
	DependencyUnresolved  = Code{"dependency_unresolved", 409, "A required mod dependency is missing."}
	Interrupted           = Code{"interrupted", 0, "The panel stopped while this job was running."}
	Timeout               = Code{"timeout", 504, "That took too long to answer."}
	Stalled               = Code{"stalled", 0, "This job stopped making progress."}

	Internal    = Code{"internal", 500, "Something went wrong."}
	Unavailable = Code{"unavailable", 503, "The panel cannot do that right now."}
)

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
// request id. Generic message out, real cause into the log: a wrapped
// "open /srv/valmin/instances/…: permission denied" is exactly what the operator needs and
// exactly what a member with one grant must not receive (D10).
//
// The request id comes from the X-Request-Id header the chain has already set, rather than
// from the request context, so this package does not import the middleware that writes it.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	e := As(err)
	if e.Code.status == 0 {
		// A job-only code, or a zero Code that escaped a struct literal. Neither has an
		// HTTP meaning, and answering with a blank status would hide the bug.
		slog.ErrorContext(r.Context(), "error code has no HTTP status",
			slog.String("code", e.Code.name), slog.String("path", r.URL.Path))
		e = New(Internal).Wrap(err)
	}

	out := body{
		Code:      e.Code,
		Message:   e.Message,
		Details:   e.Details,
		RequestID: w.Header().Get("X-Request-Id"),
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
		slog.String("path", r.URL.Path),
		slog.String("request_id", out.RequestID),
		slog.Any("error", err))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.Code.status)
	if err := json.NewEncoder(w).Encode(envelope{Error: out}); err != nil {
		slog.ErrorContext(r.Context(), "writing error envelope", slog.Any("error", err))
	}
}
