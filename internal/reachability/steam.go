package reachability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
)

// gameAppID is the app id a Valheim server registers under with Steam.
const gameAppID = 892970

const maxSteamBytes = 1 << 20

// serversAtAddress is the part of ISteamApps/GetServersAtAddress the check reads.
type serversAtAddress struct {
	Response struct {
		Success bool `json:"success"`
		Servers []struct {
			Addr     string `json:"addr"`
			AppID    int    `json:"appid"`
			GamePort int    `json:"gameport"`
		} `json:"servers"`
	} `json:"response"`
}

// steamListing asks Steam whether a Valheim server registered at ip on basePort or its query
// port.
func (c *Checker) steamListing(ctx context.Context, ip netip.Addr, basePort int) listing {
	found, err := c.registered(ctx, ip, basePort)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "steam server listing failed", slog.Any("error", err))
		return listingFailed
	case found:
		return listingFound
	default:
		return listingMissing
	}
}

func (c *Checker) registered(ctx context.Context, ip netip.Addr, basePort int) (bool, error) {
	endpoint := c.SteamURL + "/ISteamApps/GetServersAtAddress/v1/?" + url.Values{"addr": {ip.String()}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("ask Steam: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("steam answered %s", resp.Status)
	}
	var body serversAtAddress
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSteamBytes)).Decode(&body); err != nil {
		return false, fmt.Errorf("decode Steam's answer: %w", err)
	}
	if !body.Response.Success {
		return false, fmt.Errorf("steam reported no success")
	}
	for _, s := range body.Response.Servers {
		if s.AppID == gameAppID && listedOnPorts(s.GamePort, s.Addr, basePort) {
			return true, nil
		}
	}
	return false, nil
}

// listedOnPorts reports whether a Steam entry is on the server's game or query port, read
// from the entry's game port or from the port in its address.
func listedOnPorts(gamePort int, addr string, basePort int) bool {
	onServer := func(port int) bool { return port == basePort || port == basePort+1 }
	if onServer(gamePort) {
		return true
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(p)
	return err == nil && onServer(port)
}
