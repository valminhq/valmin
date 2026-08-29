package middleware

import (
	"context"
	"net/netip"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxClientIP
	ctxSessionID
)

// RequestIDFrom returns the id RequestID stamped on this request, or "" outside the chain.
// Code that only has a ResponseWriter reads the X-Request-Id header instead; both carry
// the same value.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// ClientIPFrom returns the caller's address as resolved by 10 §5. The zero Addr means the
// peer could not be parsed, which is not a reason to trust a header instead.
func ClientIPFrom(ctx context.Context) netip.Addr {
	addr, _ := ctx.Value(ctxClientIP).(netip.Addr)
	return addr
}

// WithSessionID attaches the authenticated session. Session authentication sits between
// RateLimit and CSRF in the chain; CSRF reads what it puts here.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxSessionID, id)
}

// SessionIDFrom returns the authenticated session id, or "" when the caller has none.
func SessionIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxSessionID).(string)
	return id
}
