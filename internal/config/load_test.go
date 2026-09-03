package config

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

// TestWarnsWhenCookiesCannotBeStored covers the misconfiguration whose symptom is a login
// that succeeds and achieves nothing: `Secure` cookies over plain http on a non-localhost
// host are dropped by the browser before they are ever stored.
//
// `↯` The assertion is on the *set* of hosts that warn, not on the wording of the log line,
// because the value of this check is entirely in which cases it fires. localhost has to stay
// silent — it is how every developer runs the panel — and a bare LAN IP has to warn, because
// it is how everyone first tries to reach the panel from another machine. Reported 3 Sep
// 2026 on exactly that path.
func TestWarnsWhenCookiesCannotBeStored(t *testing.T) {
	tests := []struct {
		url  string
		warn bool
	}{
		{"http://localhost:5173", false},
		{"http://127.0.0.1:8080", false},
		{"http://[::1]:8080", false},
		{"https://valmin.example.com", false},
		{"https://192.168.1.135:8080", false},
		{"http://192.168.1.135:8080", true},
		{"http://valmin.lan:8080", true},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			if err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			warnIfCookiesCannotBeStored(u)
			slog.SetDefault(prev)

			if got := strings.Contains(buf.String(), "refuse to store"); got != tt.warn {
				t.Errorf("warned = %v, want %v (logged: %q)", got, tt.warn, buf.String())
			}
		})
	}
}
