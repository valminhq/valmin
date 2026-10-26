// Package reachability tells an operator whether players outside the host can find a game
// server: it looks up the host's public address, asks Steam whether a server is registered
// there and sends the server a query from the panel host. It runs from inside the panel
// container, so it cannot see the router's own address and says what it could not tell.
package reachability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SteamAPI is the host of Steam's public web API.
const SteamAPI = "https://api.steampowered.com"

const (
	lookupTimeout = 5 * time.Second
	probeTimeout  = 7 * time.Second
	maxIPBytes    = 64
)

// unroutable lists the ranges that pass the global unicast and private checks yet are not a
// host's public address: this-network, shared (carrier-grade NAT and overlay networks),
// protocol assignments, benchmarking and the reserved block.
var unroutable = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Status is a verdict or one step's outcome. Unknown is distinct from ok: a fact that could
// not be measured must not read as a pass.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusUnknown Status = "unknown"
)

// Step is one measurement behind a verdict.
type Step struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the answer to one check. PublicAddress is where players connect directly, empty
// when the host's address is unknown or the server joins by code.
type Report struct {
	Status        Status    `json:"status"`
	PublicAddress string    `json:"public_address,omitempty"`
	Detail        string    `json:"detail"`
	Remedy        string    `json:"remedy,omitempty"`
	Steps         []Step    `json:"steps"`
	CheckedAt     time.Time `json:"checked_at"`
}

// Target is the server being checked. BasePort is its game port; the query port is the next.
type Target struct {
	BasePort  int
	Public    bool
	Crossplay bool
	Running   bool
	// RestartPending is set when a settings change waits for a restart, so the running server
	// may not match the settings it is checked against.
	RestartPending bool
	// JoinCode is the crossplay session's code, empty until the server has logged one.
	JoinCode string
}

// Checker runs checks. SteamURL is the base URL of the Steam web API and PublicIPURL the
// service that answers with the caller's address as plain text, empty to skip the lookup.
type Checker struct {
	HTTP        *http.Client
	PublicIPURL string
	SteamURL    string

	// query sends the server query; nil sends it over UDP.
	query func(ctx context.Context, hostPort string) error
}

// New returns a Checker that asks Steam's API and the public IP service at publicIPURL.
func New(client *http.Client, publicIPURL string) *Checker {
	return &Checker{HTTP: client, PublicIPURL: publicIPURL, SteamURL: SteamAPI}
}

// listing is what Steam said about the server's registration.
type listing int

const (
	listingNotAsked listing = iota
	listingFailed
	listingFound
	listingMissing
)

// facts is everything verdict reads.
type facts struct {
	Target
	ip       netip.Addr
	answered bool
	listing  listing
}

// Check measures t and returns the verdict. It never fails: a probe that errors becomes a
// step the report names.
func (c *Checker) Check(ctx context.Context, t Target) Report {
	f := facts{Target: t}
	steps := []Step{}
	if t.Running && !t.RestartPending && !t.Crossplay {
		steps = c.measure(ctx, &f)
	}
	r := verdict(f)
	r.Steps = steps
	r.CheckedAt = time.Now().UTC()
	return r
}

// measure fills f from the network and returns the steps it took. A server answers Steam
// queries only while public listing is on, so any other server is not queried. The Steam
// listing and the server query run side by side, with a time budget of their own so a slow
// lookup never leaves the query too little time to be answered.
func (c *Checker) measure(ctx context.Context, f *facts) []Step {
	lookupCtx, cancelLookup := context.WithTimeout(ctx, lookupTimeout)
	ip, ipStep := c.lookupIP(lookupCtx)
	cancelLookup()
	steps := []Step{ipStep}
	if !ip.IsValid() {
		return steps
	}
	f.ip = ip

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if !f.Public {
		return append(steps, Step{
			Name: "server_query", Status: StatusUnknown,
			Detail: "Not tested: a server answers queries only while public listing is on.",
		})
	}

	query := net.JoinHostPort(ip.String(), strconv.Itoa(f.BasePort+1))
	var queryErr error
	var wg sync.WaitGroup
	wg.Go(func() { f.listing = c.steamListing(ctx, ip, f.BasePort) })
	wg.Go(func() { queryErr = c.ask(ctx, query) })
	wg.Wait()

	f.answered = queryErr == nil
	return append(steps, steamStep(f.listing, ip), queryStep(query, queryErr))
}

// ask sends the server query to hostPort.
func (c *Checker) ask(ctx context.Context, hostPort string) error {
	if c.query != nil {
		return c.query(ctx, hostPort)
	}
	return answers(ctx, hostPort)
}

// lookupIP asks the public IP service for the host's IPv4 address. The result is the zero
// Addr when the lookup is off or failed.
func (c *Checker) lookupIP(ctx context.Context) (netip.Addr, Step) {
	step := Step{Name: "public_ip"}
	if c.PublicIPURL == "" {
		step.Status, step.Detail = StatusUnknown, "The public IP lookup is turned off."
		return netip.Addr{}, step
	}
	ip, err := c.fetchIP(ctx)
	if err != nil {
		slog.WarnContext(ctx, "public ip lookup failed", slog.Any("error", err))
		step.Status, step.Detail = StatusWarn, "The public IP lookup did not return an address."
		return netip.Addr{}, step
	}
	step.Status, step.Detail = StatusOK, "The host's public address is "+ip.String()+"."
	return ip, step
}

// fetchIP reads one public IPv4 address from PublicIPURL. An address that is not a public
// one is refused, so a faulty service cannot point the query at the host's own network.
func (c *Checker) fetchIP(ctx context.Context) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PublicIPURL, http.NoBody)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("ask the lookup service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("lookup service answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIPBytes))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read the lookup answer: %w", err)
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse the lookup answer: %w", err)
	}
	if !publicIPv4(ip) {
		return netip.Addr{}, fmt.Errorf("lookup answered %s, not a public IPv4 address", ip)
	}
	return ip, nil
}

// publicIPv4 reports whether ip can be a host's address on the public internet.
func publicIPv4(ip netip.Addr) bool {
	return ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() &&
		!slices.ContainsFunc(unroutable, func(p netip.Prefix) bool { return p.Contains(ip) })
}

func steamStep(l listing, ip netip.Addr) Step {
	step := Step{Name: "steam_listing"}
	switch l {
	case listingFound:
		step.Status, step.Detail = StatusOK, "Steam lists this server at "+ip.String()+"."
	case listingMissing:
		step.Status, step.Detail = StatusWarn, "Steam does not list this server at "+ip.String()+"."
	case listingFailed, listingNotAsked:
		step.Status, step.Detail = StatusUnknown, "Steam did not answer."
	}
	return step
}

func queryStep(query string, err error) Step {
	step := Step{Name: "server_query"}
	if err != nil {
		step.Status, step.Detail = StatusWarn, "No answer from "+query+" (UDP) when asked from the panel host."
		return step
	}
	step.Status, step.Detail = StatusOK, query+" (UDP) answered when asked from the panel host."
	return step
}

// verdict turns the measurements into one answer and what to do about it.
func verdict(f facts) Report {
	switch {
	case !f.Running:
		return Report{
			Status: StatusUnknown,
			Detail: "The server is not running.",
			Remedy: "Start the server, then check again.",
		}
	case f.RestartPending:
		return Report{
			Status: StatusUnknown,
			Detail: "A settings change is waiting for a restart, so the running server may not match its settings.",
			Remedy: "Restart the server, then check again.",
		}
	case f.Crossplay && f.JoinCode == "":
		return Report{
			Status: StatusWarn,
			Detail: "Crossplay is on, but the server has not logged a join code yet.",
			Remedy: "The code appears a few seconds after the server starts. Check again; if it never " +
				"appears, look for errors in the console.",
		}
	case f.Crossplay:
		return Report{
			Status: StatusOK,
			Detail: "Crossplay is on and the session is registered. Players join with the join code.",
		}
	case !f.ip.IsValid():
		return Report{
			Status: StatusUnknown,
			Detail: "The host's public address could not be determined.",
			Remedy: "Set game.public_ip_url to a service that answers with the caller's address, or " +
				"check that the panel can reach the one configured.",
		}
	}

	join := net.JoinHostPort(f.ip.String(), strconv.Itoa(f.BasePort))
	r := Report{PublicAddress: join}
	switch {
	case !f.Public:
		r.Status = StatusUnknown
		r.Detail = "A server answers Steam queries only while public listing is on, so its address " +
			join + " could not be tested."
		r.Remedy = "Turn on public listing and restart the server to test it, or ask a friend " +
			"outside your network to join at " + join + "."
	case !f.answered:
		r.Status = StatusWarn
		r.Detail = "The server did not answer at " + join + " when asked from the panel host."
		r.Remedy = unreachableRemedy(f.ip, f.BasePort)
	case f.listing == listingMissing:
		r.Status = StatusWarn
		r.Detail = "The server answers at " + join + ", but Steam does not list it."
		r.Remedy = "The server registers with Steam some time after it starts; check again shortly. If it " +
			"stays missing, make sure the host can reach Steam over the internet."
	default:
		r.Status = StatusOK
		r.Detail = "The server answers at " + join + ". Players connect to that address."
	}
	return r
}

// unreachableRemedy lists the causes of a server that does not answer on its public address,
// most common first.
func unreachableRemedy(ip netip.Addr, basePort int) string {
	ports := strconv.Itoa(basePort) + "-" + strconv.Itoa(basePort+1)
	return "Forward UDP ports " + ports + " on the router to this host and allow them in the host's " +
		"firewall. If both are already done, the router may lack NAT loopback, which hides a working " +
		"server from the panel host: ask a friend outside your network to join at " +
		net.JoinHostPort(ip.String(), strconv.Itoa(basePort)) + ". If the router's own internet " +
		"address differs from " + ip.String() + " or is in 100.64.0.0/10, the connection is behind " +
		"carrier-grade NAT and forwarding cannot work: turn on crossplay and share the join code instead."
}
