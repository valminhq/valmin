package api

import (
	"time"

	"github.com/valminhq/valmin/internal/ratelimit"
)

// Request limits per minute, each with its burst. An address key is the client's IPv4 address
// or IPv6 /64; the login limit per name is keyed on the name at one address, so nobody can
// spend another person's attempts.
const (
	floodPerMinute, floodBurst               = 300, 100 // every API request, per address and per user
	setupPerMinute, setupBurst               = 5, 5     // first-run setup, per address
	loginPerMinute, loginBurst               = 10, 10   // login, per address
	loginNamePerMinute, loginNameBurst       = 5, 5     // login, per username at one address
	passwordPerMinute, passwordBurst         = 5, 5     // own password change, per user
	redeemPerMinute, redeemBurst             = 10, 10   // invite redemption, per address
	publicStatusPerMinute, publicStatusBurst = 60, 20   // the public status page, per address
)

// perMinute is a limiter of n requests a minute with the given burst.
func perMinute(n, burst int) *ratelimit.Limiter { return ratelimit.New(n, time.Minute, burst) }
