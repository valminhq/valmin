package reachability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	queryAttempts  = 3
	attemptTimeout = 2 * time.Second
	maxQueryReply  = 1400
	headerLen      = 4
)

// infoRequest is Valve's A2S_INFO query: a four byte 0xFF header, 'T' and a fixed string.
const infoRequest = "\xFF\xFF\xFF\xFFTSource Engine Query\x00"

// answers sends an A2S_INFO query to hostPort over UDP and returns nil as soon as the server
// replies. A server may answer with its info or with a challenge asking the sender to prove
// its address; either shows the query arrived, which is all that is being tested. Silence is
// retried up to queryAttempts times, because a lost datagram looks the same as a closed port.
// The query stops when ctx ends.
func answers(ctx context.Context, hostPort string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", hostPort)
	if err != nil {
		return fmt.Errorf("dial %s: %w", hostPort, err)
	}
	defer func() { _ = conn.Close() }()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	for range queryAttempts {
		isQueryReply, err := exchange(ctx, conn)
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil:
			// Silence: ask again.
		case err != nil:
			return fmt.Errorf("query %s: %w", hostPort, err)
		case !isQueryReply:
			return fmt.Errorf("unexpected reply from %s", hostPort)
		default:
			return nil
		}
	}
	return fmt.Errorf("no reply from %s after %d queries", hostPort, queryAttempts)
}

// exchange sends one query and reads one reply, reporting whether the reply is one a Source
// query server sends. The read ends at attemptTimeout or at ctx's deadline, whichever is first.
func exchange(ctx context.Context, conn net.Conn) (isQueryReply bool, err error) {
	if _, err := conn.Write([]byte(infoRequest)); err != nil {
		return false, contextOr(ctx, fmt.Errorf("send: %w", err))
	}
	deadline := time.Now().Add(attemptTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return false, fmt.Errorf("set read deadline: %w", err)
	}
	reply := make([]byte, maxQueryReply)
	n, err := conn.Read(reply)
	if err != nil {
		return false, contextOr(ctx, fmt.Errorf("read: %w", err))
	}
	return sourceReply(reply[:n]), nil
}

// contextOr returns the context's own error when it ended the exchange, else err.
func contextOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("query ended: %w", ctx.Err())
	}
	return err
}

// sourceReply reports whether p is a reply to a query: a single packet carrying the server's
// info ('I') or a challenge ('A'), or the first packet of a reply sent in several.
func sourceReply(p []byte) bool {
	switch {
	case bytes.HasPrefix(p, []byte("\xFE\xFF\xFF\xFF")):
		return true
	case len(p) > headerLen && bytes.HasPrefix(p, []byte("\xFF\xFF\xFF\xFF")):
		return p[headerLen] == 'I' || p[headerLen] == 'A'
	default:
		return false
	}
}
