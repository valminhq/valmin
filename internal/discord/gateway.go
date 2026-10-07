package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Gateway opcodes the bot uses.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// gatewayReadLimit bounds one Gateway message. READY is the largest the bot receives.
const gatewayReadLimit = 1 << 20

// frame is one Gateway message as received.
type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// outbound is one Gateway message as sent.
type outbound struct {
	Op int `json:"op"`
	D  any `json:"d"`
}

// fatalError is a Gateway close that reconnecting cannot fix, such as a rejected token.
type fatalError struct {
	code websocket.StatusCode
	msg  string
}

func (e *fatalError) Error() string { return fmt.Sprintf("%s (close code %d)", e.msg, e.code) }

// fatalCloses are the close codes that end the bot until its settings change.
var fatalCloses = map[websocket.StatusCode]string{
	4004: "Discord rejected the bot token",
	4010: "Discord rejected the shard settings",
	4011: "Discord requires sharding for this bot",
	4012: "Discord rejected the Gateway version",
	4013: "Discord rejected the requested intents",
	4014: "Discord refused the requested intents",
}

// errReconnect asks the run loop for a fresh connection straight away; errInvalidSession asks
// for one after the usual backoff.
var (
	errReconnect      = errors.New("discord asked the bot to reconnect")
	errInvalidSession = errors.New("discord invalidated the session")
)

// session holds one Gateway connection from Hello until it drops. root outlives the
// connection and is what command handlers run under. It reports whether READY arrived.
func (b *Bot) session(root, ctx context.Context, token string) (ready bool, err error) {
	conn, interval, err := b.connect(ctx, token)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.CloseNow() }()

	var seq atomic.Int64
	seq.Store(-1)
	var acked atomic.Bool
	acked.Store(true)
	beat := func() error {
		var d any
		if s := seq.Load(); s >= 0 {
			d = s
		}
		return wsjson.Write(ctx, conn, outbound{Op: opHeartbeat, D: d})
	}
	go b.heartbeat(ctx, conn, interval, &acked, beat)

	for {
		f, err := readFrame(ctx, conn)
		if err != nil {
			return ready, err
		}
		if f.S != nil {
			seq.Store(*f.S)
		}
		if f.Op == opDispatch && f.T == "READY" {
			ready = true
		}
		if err := b.onFrame(root, &f, token, &acked, beat); err != nil {
			return ready, err
		}
	}
}

// connect dials the Gateway, waits for Hello and identifies. It returns the heartbeat interval.
func (b *Bot) connect(ctx context.Context, token string) (*websocket.Conn, time.Duration, error) {
	conn, _, err := websocket.Dial(ctx, b.gatewayURL, nil) //nolint:bodyclose // closed by the library
	if err != nil {
		return nil, 0, fmt.Errorf("connect to the Discord Gateway: %w", err)
	}
	conn.SetReadLimit(gatewayReadLimit)
	var hello struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	first, err := readFrame(ctx, conn)
	if err == nil && (first.Op != opHello || json.Unmarshal(first.D, &hello) != nil || hello.HeartbeatInterval <= 0) {
		err = errors.New("the Discord Gateway did not say hello")
	}
	if err == nil {
		identify := map[string]any{
			"token":      token,
			"intents":    0,
			"properties": map[string]string{"os": "linux", "browser": "valmin", "device": "valmin"},
		}
		if werr := wsjson.Write(ctx, conn, outbound{Op: opIdentify, D: identify}); werr != nil {
			err = fmt.Errorf("identify to Discord: %w", werr)
		}
	}
	if err != nil {
		_ = conn.CloseNow()
		return nil, 0, err
	}
	return conn, time.Duration(hello.HeartbeatInterval) * time.Millisecond, nil
}

// onFrame acts on one Gateway message. A non-nil error ends the session.
func (b *Bot) onFrame(root context.Context, f *frame, token string, acked *atomic.Bool, beat func() error) error {
	switch f.Op {
	case opDispatch:
		switch f.T {
		case "READY":
			b.ready(root, f.D, token)
		case "INTERACTION_CREATE":
			go b.handle(root, f.D)
		}
	case opHeartbeat:
		if err := beat(); err != nil {
			return fmt.Errorf("heartbeat to Discord: %w", err)
		}
	case opHeartbeatACK:
		acked.Store(true)
	case opReconnect:
		return errReconnect
	case opInvalidSession:
		return errInvalidSession
	}
	return nil
}

// heartbeat sends a heartbeat every interval, the first after a random fraction of it, and
// closes the connection when Discord did not acknowledge the previous one.
func (b *Bot) heartbeat(
	ctx context.Context, conn *websocket.Conn, interval time.Duration, acked *atomic.Bool, beat func() error,
) {
	timer := time.NewTimer(time.Duration(rand.Float64() * float64(interval))) //nolint:gosec // jitter
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !acked.Swap(false) {
			_ = conn.Close(websocket.StatusCode(4000), "heartbeat not acknowledged")
			return
		}
		if err := beat(); err != nil {
			return
		}
		timer.Reset(interval)
	}
}

// readFrame reads one Gateway message, turning a fatal close into a fatalError.
func readFrame(ctx context.Context, conn *websocket.Conn) (frame, error) {
	var f frame
	err := wsjson.Read(ctx, conn, &f)
	if err == nil {
		return f, nil
	}
	code := websocket.CloseStatus(err)
	if msg, ok := fatalCloses[code]; ok {
		return f, &fatalError{code: code, msg: msg}
	}
	return f, fmt.Errorf("read from the Discord Gateway: %w", err)
}

// ready records who the bot is and registers its commands in every linked Discord server.
func (b *Bot) ready(ctx context.Context, raw json.RawMessage, token string) {
	var r struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		Application struct {
			ID string `json:"id"`
		} `json:"application"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		slog.WarnContext(ctx, "read Discord READY", slog.Any("error", err))
		return
	}
	b.setStatus(Status{State: StateConnected, BotName: r.User.Username, ApplicationID: r.Application.ID})
	go b.registerCommands(ctx, r.Application.ID, token)
}
