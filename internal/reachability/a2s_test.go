package reachability

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// serveUDP answers each datagram with whatever handle returns, and returns the address it
// listens on. A nil result is silence.
func serveUDP(t *testing.T, handle func(request []byte) []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := handle(buf[:n]); reply != nil {
				_, _ = conn.WriteTo(reply, from)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func reply(kind byte, payload ...byte) []byte {
	return append([]byte{0xFF, 0xFF, 0xFF, 0xFF, kind}, payload...)
}

func TestAnswers(t *testing.T) {
	tests := []struct {
		name    string
		reply   []byte
		wantErr bool
	}{
		{name: "info", reply: reply('I', 17, 'V')},
		{name: "a challenge is an answer", reply: reply('A', 1, 2, 3, 4)},
		{name: "a reply in several packets", reply: []byte("\xFE\xFF\xFF\xFFpart")},
		{name: "another kind of reply", reply: reply('m', 1), wantErr: true},
		{name: "no header", reply: []byte("not a source reply"), wantErr: true},
		{name: "a header and nothing else", reply: []byte("\xFF\xFF\xFF\xFF"), wantErr: true},
		{name: "silence until the context ends", reply: nil, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			addr := serveUDP(t, func([]byte) []byte { return tc.reply })
			err := answers(ctx, addr)
			if (err != nil) != tc.wantErr {
				t.Errorf("answers() error = %v, want error: %t", err, tc.wantErr)
			}
		})
	}
}

// Every query is the bare info request, and a lost datagram is asked again.
func TestAnswersAsksAgainAfterSilence(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	addr := serveUDP(t, func(request []byte) []byte {
		if string(request) != infoRequest {
			t.Errorf("request = %q, want the bare info request", request)
		}
		if requests.Add(1) == 1 {
			return nil
		}
		return reply('I')
	})
	if err := answers(t.Context(), addr); err != nil {
		t.Fatalf("answers() = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2: one lost, one answered", got)
	}
}

// Cancelling the context ends a read that is waiting, with no deadline to end it.
func TestAnswersStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	addr := serveUDP(t, func([]byte) []byte { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := answers(ctx, addr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("answers() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > attemptTimeout/2 {
		t.Errorf("answers() took %s after the cancel, want it to stop at once", elapsed)
	}
}

func TestSourceReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want bool
	}{
		{"info", reply('I'), true},
		{"challenge", reply('A', 1, 2, 3, 4), true},
		{"split", []byte("\xFE\xFF\xFF\xFF\x01"), true},
		{"empty", nil, false},
		{"header only", []byte("\xFF\xFF\xFF\xFF"), false},
		{"other kind", reply('m'), false},
		{"wrong header", append([]byte{0xFF, 0xFF, 0xFF, 0x00}, 'I'), false},
	} {
		if got := sourceReply(tc.in); got != tc.want {
			t.Errorf("%s: sourceReply(%q) = %t, want %t", tc.name, bytes.Clone(tc.in), got, tc.want)
		}
	}
}
