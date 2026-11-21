package api

import (
	"net"
	"strconv"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// maxJoinHost is the longest DNS name.
const maxJoinHost = 253

// steamRunValheim launches the Valheim client through Steam with the launch arguments that
// follow it.
const steamRunValheim = "steam://run/892970//"

// validJoinHost reports whether host is an IP address or a DNS name of letters, digits and
// hyphens. Anything else, such as a port, a scheme or a space, is refused.
func validJoinHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > maxJoinHost {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if !validHostLabel(label) {
			return false
		}
	}
	return true
}

// validHostLabel reports whether label is one DNS label: 1 to 63 letters, digits and hyphens,
// neither starting nor ending with a hyphen.
func validHostLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, c := range label {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// addJoinHostViolations rejects a join host that is neither empty nor a valid host.
func addJoinHostViolations(val *apierr.Validation, body *patchInstanceRequest) {
	if v := body.JoinHost; v != nil {
		if host := strings.TrimSpace(*v); host != "" && !validJoinHost(host) {
			val.Add("join_host", apierr.FieldInvalid,
				"Enter a host name or IP address without a port, such as play.example.com.")
		}
	}
}

// joinAddress is the address players type to join: the join host with the server's game port,
// or nil when no join host is set.
func joinAddress(host string, basePort int) *string {
	if host == "" {
		return nil
	}
	addr := net.JoinHostPort(host, strconv.Itoa(basePort))
	return &addr
}

// steamJoinURL is the link that starts Valheim through Steam and connects it to addr, or nil
// when there is no address, the address is IPv6, or the server joins by crossplay code. addr
// is a validated host and a port, so it holds no character that needs escaping.
func steamJoinURL(addr *string, crossplay bool) *string {
	if addr == nil || crossplay || strings.HasPrefix(*addr, "[") {
		return nil
	}
	link := steamRunValheim + "+connect%20" + *addr
	return &link
}
