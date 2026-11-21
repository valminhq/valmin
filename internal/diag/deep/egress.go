package deep

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
)

// metadataAddress is where cloud providers serve instance metadata, credentials included.
var metadataAddress = netip.MustParseAddrPort("169.254.169.254:80")

const (
	egressPurpose = "egress-check"
	// egressSeconds is how long the probe waits for a connection before `timeout` ends it.
	egressSeconds = 3
	// exitTimedOut is the code `timeout` exits with when it ends the command.
	exitTimedOut = 124
	// refusedLiteral is bash's message for a refused /dev/tcp connect, measured in the game
	// image.
	refusedLiteral = "Connection refused"
)

// egressResult is what one connection attempt from the game network saw.
type egressResult int

const (
	egressConnected egressResult = iota + 1
	egressRefused
	egressNoAnswer
	egressUnclear
)

// classifyEgress reads a probe's exit code and stderr. A refusal still means a packet got
// there; only a timeout is silence, and silence is not proof of a block.
func classifyEgress(code int, stderr string) egressResult {
	switch {
	case code == 0:
		return egressConnected
	case code == exitTimedOut:
		return egressNoAnswer
	case code == 1 && strings.Contains(stderr, refusedLiteral):
		return egressRefused
	default:
		return egressUnclear
	}
}

// egressTarget is one address the deep job tries to reach, and how its outcome is worded.
type egressTarget struct {
	addr netip.AddrPort
	// name is how the detail refers to the address. A LAN address stays out of it, since the
	// detail travels in the support bundle.
	name string
	// reachedNote and silentNote extend the detail when the address answered or stayed silent.
	reachedNote, silentNote string
}

var metadataTarget = egressTarget{
	addr:        metadataAddress,
	name:        "the cloud metadata service (169.254.169.254:80)",
	reachedNote: " On a cloud VPS, a mod can read the machine's credentials from it.",
	silentNote:  " Outside a cloud this is expected.",
}

func lanTarget(addr netip.AddrPort) *egressTarget {
	return &egressTarget{addr: addr, name: "the LAN address"}
}

// probeEgresses tries the metadata service and, when one is configured, the LAN address, and
// adds each outcome to observed under its KV key.
func probeEgresses(
	ctx context.Context,
	jh *jobs.Handle,
	rt runtime.Runtime,
	cfg *config.Config,
	observed map[string]diag.Observation,
) {
	egress := func(key string, t *egressTarget) {
		obs := probeEgress(ctx, rt, cfg, t)
		if !obs.OK {
			jh.Log(obs.Detail)
		}
		observed[key] = obs
	}
	jh.Progress(ctx, 60, "Trying the cloud metadata service from the game network")
	egress(diag.MetadataEgressKey, &metadataTarget)
	// An empty address, the only invalid one Load lets through, skips the LAN probe.
	if addr, err := config.ParseLANProbeAddress(cfg.Game.LANProbeAddress); err == nil {
		jh.Progress(ctx, 70, "Trying the LAN address from the game network")
		egress(diag.LANEgressKey, lanTarget(addr))
	}
}

// probeEgress opens one TCP connection to t from a throwaway container in the game image on
// the game network, set up as a game container is, and grades the outcome.
func probeEgress(ctx context.Context, rt runtime.Runtime, cfg *config.Config, t *egressTarget) diag.Observation {
	var stderr strings.Builder
	code, err := runtime.RunThrowaway(ctx, rt, &runtime.ThrowawaySpec{
		Purpose: egressPurpose,
		Root:    cfg.Data.HostRoot,
		Image:   cfg.Game.Image,
		// The address arrives as the script's arguments, never as part of its text.
		Entrypoint: []string{
			"/usr/bin/timeout", strconv.Itoa(egressSeconds), "/bin/bash", "-c", "</dev/tcp/$0/$1",
			t.addr.Addr().String(), strconv.Itoa(int(t.addr.Port())),
		},
		User:       strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Network:    cfg.Game.Network,
		OCIRuntime: cfg.Game.Runtime,
		Binds:      cfg.GameResolvBinds(),
		Stderr:     &stderr,
	})
	obs := diag.Observation{CheckedAt: time.Now().UTC(), Source: diag.SourceJob, Target: t.addr.String()}
	if err != nil {
		obs.Status, obs.Detail, obs.Diagnostic = diag.StatusUnknown, "The probe could not run.", err.Error()
		return obs
	}
	where := t.name
	switch classifyEgress(code, stderr.String()) {
	case egressConnected:
		obs.Status = diag.StatusWarn
		obs.Detail = "A game server opened a connection to " + where + "." + t.reachedNote
	case egressRefused:
		obs.Status = diag.StatusWarn
		obs.Detail = strings.ToUpper(where[:1]) + where[1:] + " refused the connection, so packets from " +
			"game servers reach it. A REJECT firewall rule on the way gives the same answer."
	case egressNoAnswer:
		obs.Status, obs.OK = diag.StatusOK, true
		obs.Detail = fmt.Sprintf("No answer from %s within %d seconds. This is not proof that it is "+
			"blocked: nothing may be listening there.%s", where, egressSeconds, t.silentNote)
	case egressUnclear:
		obs.Status = diag.StatusUnknown
		obs.Detail = fmt.Sprintf("The probe of %s exited %d without a recognised answer.", where, code)
		obs.Diagnostic = strings.TrimSpace(stderr.String())
	}
	return obs
}
