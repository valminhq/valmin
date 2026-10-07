// Package discord is the panel's Discord bot: one Gateway connection that answers /status and
// /start in the Discord channels an administrator linked to chosen servers.
package discord

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// The bot's connection states, as the admin page shows them.
const (
	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateFailed     = "failed"
)

const (
	defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	defaultAPIBase    = "https://discord.com/api/v10"
	minBackoff        = time.Second
	maxBackoff        = 2 * time.Minute
)

// Status is what the bot is doing now.
type Status struct {
	State         string `json:"state"`
	BotName       string `json:"bot_name,omitempty"`
	ApplicationID string `json:"application_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

// starter submits a start job.
type starter interface {
	Submit(ctx context.Context, input *control.StartSubmission) (*store.Job, error)
}

// Bot runs the Discord bot for the life of the process.
type Bot struct {
	DB      *store.DB
	Keeper  *crypto.Keeper
	Starter starter
	// Players is an instance's live player count, nil when the panel cannot say.
	Players func(instanceID string) *int

	gatewayURL string
	apiBase    string
	client     *http.Client
	followFor  time.Duration
	followTick time.Duration

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	wake   chan struct{}
}

// New builds a bot against the real Discord endpoints.
func New(db *store.DB, keeper *crypto.Keeper, s starter, players func(string) *int) *Bot {
	return &Bot{
		DB: db, Keeper: keeper, Starter: s, Players: players,
		gatewayURL: defaultGatewayURL, apiBase: defaultAPIBase,
		client:    &http.Client{Timeout: 15 * time.Second},
		followFor: 10 * time.Minute, followTick: 5 * time.Second,
		status: Status{State: StateDisabled},
		wake:   make(chan struct{}, 1),
	}
}

// Status reports the bot's current state.
func (b *Bot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Bot) setStatus(s Status) {
	b.mu.Lock()
	b.status = s
	b.mu.Unlock()
}

// Reload drops the current connection, if any, and starts again from the stored settings.
func (b *Bot) Reload() {
	b.mu.Lock()
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run connects while the bot is configured and enabled, reconnecting with backoff, until ctx
// is cancelled.
func (b *Bot) Run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		sessCtx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		b.cancel = cancel
		b.mu.Unlock()
		select {
		case <-b.wake:
		default:
		}

		token, status := b.token(ctx)
		if token == "" {
			cancel()
			b.setStatus(status)
			b.sleep(ctx, 0)
			continue
		}
		b.setStatus(Status{State: StateConnecting})
		ready, err := b.session(ctx, sessCtx, token)
		reloaded := sessCtx.Err() != nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case reloaded, errors.Is(err, errReconnect):
			backoff = minBackoff
			continue
		}
		var fatal *fatalError
		if errors.As(err, &fatal) {
			b.setStatus(Status{State: StateFailed, Error: fatal.Error()})
			b.sleep(ctx, 0)
			continue
		}
		if ready {
			backoff = minBackoff
		}
		slog.InfoContext(ctx, "discord connection lost, will reconnect", slog.Any("error", err))
		b.setStatus(Status{State: StateConnecting, Error: errorText(err)})
		b.sleep(ctx, backoff/2+time.Duration(rand.Int64N(int64(backoff/2)+1))) //nolint:gosec // jitter
		backoff = min(backoff*2, maxBackoff)
	}
}

// sleep waits d, or until Reload or ctx ends it. A zero d waits only for those two.
func (b *Bot) sleep(ctx context.Context, d time.Duration) {
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
	case <-b.wake:
	case <-timeout:
	}
}

// token reads and opens the stored token. An empty token comes with the status explaining why.
func (b *Bot) token(ctx context.Context) (string, Status) {
	bot, err := b.DB.DiscordBot(ctx)
	if err != nil {
		slog.WarnContext(ctx, "read discord bot settings", slog.Any("error", err))
		return "", Status{State: StateFailed, Error: "The bot settings could not be read."}
	}
	if bot == nil || bot.Token == "" || !bot.Enabled {
		return "", Status{State: StateDisabled}
	}
	plain, err := b.Keeper.Decrypt(crypto.PurposeDiscordToken, crypto.DiscordTokenLocation(bot.ID), bot.Token)
	if err != nil {
		slog.WarnContext(ctx, "open discord bot token", slog.Any("error", err))
		return "", Status{State: StateFailed, Error: "The stored bot token cannot be read. Paste it again."}
	}
	return string(plain), Status{}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
