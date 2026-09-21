package diag

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/config"
)

// bundleTime is the modification time every entry carries, so two bundles of the same
// state are byte-identical.
var bundleTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// readme is the first thing a reader opens, and the contract the bundle keeps.
const readme = `Valmin support bundle

report.json    the diagnostics report: one row per check, plus per-instance state
config.json    operator settings, secrets and filesystem paths removed

Deliberately absent, so this file can be attached to a public issue unedited:

  - every secret, credential and token
  - the database DSN and the master key path
  - absolute filesystem paths; the facts derived from them are in report.json instead
  - world names and player identifiers
  - console and game log content
  - verbatim output from the probes, which can name paths and hosts; the diagnostics
    page shows it

Nothing here is redacted after the fact. Each field is copied in by name, so a value
that is not listed above was never collected (D14).
`

// ConfigView is the operator settings a bundle carries. Every field is listed by hand:
// the bundle is built for a public issue attachment, so it is an allowlist, never a copy
// of config.Config with keys removed (D14, 11 §9).
type ConfigView struct {
	ListenPort          string `json:"listen_port"`
	ExternalURLScheme   string `json:"external_url_scheme"`
	ExternalURLIsLocal  bool   `json:"external_url_is_local"`
	CookiesUnstorable   bool   `json:"cookies_unstorable"`
	TrustedProxyCount   int    `json:"trusted_proxy_count"`
	RequestTimeout      string `json:"request_timeout"`
	BodyLimitBytes      int64  `json:"body_limit_bytes"`
	ShutdownGrace       string `json:"shutdown_grace"`
	FreeSpaceFloorBytes int64  `json:"free_space_floor_bytes"`
	DBDriver            string `json:"db_driver"`
	DockerAPIVersion    string `json:"docker_api_version"`
	GameImage           string `json:"game_image"`
	SteamCMDImage       string `json:"steamcmd_image"`
	GameNetwork         string `json:"game_network"`
	StopTimeout         string `json:"stop_timeout"`
	DefaultMemMB        int    `json:"default_mem_mb"`
	PortBase            int    `json:"port_base"`
	PortStride          int    `json:"port_stride"`
	ThunderstoreBaseURL string `json:"thunderstore_base_url"`
	ThunderstoreSync    string `json:"thunderstore_sync_interval"`
	SessionIdleTTL      string `json:"session_idle_ttl"`
	SessionAbsoluteTTL  string `json:"session_absolute_ttl"`
	LogLevel            string `json:"log_level"`
	LogFormat           string `json:"log_format"`
}

// NewConfigView copies the settings a bundle may carry. data.root, data.host_root,
// db.dsn and secrets.master_key_file are absent: they name the operator's account and
// host layout, and report.json already carries what those paths were measured to be.
func NewConfigView(cfg *config.Config) *ConfigView {
	scheme, isLocal := externalURLShape(cfg.Server.ExternalURL)
	return &ConfigView{
		ListenPort:          listenPort(cfg.Server.Listen),
		ExternalURLScheme:   scheme,
		ExternalURLIsLocal:  isLocal,
		CookiesUnstorable:   config.CookiesUnstorable(cfg.Server.ExternalURL),
		TrustedProxyCount:   len(cfg.Server.TrustedProxies),
		RequestTimeout:      cfg.Server.RequestTimeout.String(),
		BodyLimitBytes:      cfg.Server.BodyLimitBytes,
		ShutdownGrace:       cfg.Server.ShutdownGrace.String(),
		FreeSpaceFloorBytes: cfg.Data.FreeSpaceFloorBytes,
		DBDriver:            cfg.DB.Driver,
		DockerAPIVersion:    cfg.Docker.APIVersion,
		GameImage:           cfg.Game.Image,
		SteamCMDImage:       cfg.Game.SteamCMDImage,
		GameNetwork:         cfg.Game.Network,
		StopTimeout:         cfg.Game.StopTimeout.String(),
		DefaultMemMB:        cfg.Game.DefaultMemMB,
		PortBase:            cfg.Ports.Base,
		PortStride:          cfg.Ports.Stride,
		ThunderstoreBaseURL: cfg.Thunderstore.BaseURL,
		ThunderstoreSync:    cfg.Thunderstore.SyncInterval.String(),
		SessionIdleTTL:      cfg.Auth.SessionIdleTTL.String(),
		SessionAbsoluteTTL:  cfg.Auth.SessionAbsoluteTTL.String(),
		LogLevel:            cfg.Log.Level,
		LogFormat:           cfg.Log.Format,
	}
}

// listenPort is the port half of a listen address, which is the part that is not the
// operator's network layout.
func listenPort(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return port
	}
	return ""
}

// externalURLShape reports the scheme and whether the host is a loopback name. The host
// itself stays out: it is the operator's domain.
func externalURLShape(raw string) (scheme string, isLocal bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	return u.Scheme, host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// forBundle is the report without the verbatim probe output, which this package did not
// compose and so cannot promise is free of paths or hostnames.
func (r *Report) forBundle() Report {
	out := *r
	out.Checks = slices.Clone(r.Checks)
	for i := range out.Checks {
		out.Checks[i].Diagnostic = ""
	}
	out.Instances = slices.Clone(r.Instances)
	for i := range out.Instances {
		if out.Instances[i].ModsError != "" {
			out.Instances[i].ModsError = "Could not read installed mods."
		}
		if out.Instances[i].InspectionError != "" {
			out.Instances[i].InspectionError = "Could not inspect the container."
		}
	}
	return out
}

// WriteBundle writes the report and settings to w as a zip archive.
func WriteBundle(w io.Writer, r *Report, cfg *ConfigView) error {
	z := zip.NewWriter(w)
	entries := []struct {
		name string
		body any
	}{
		{"report.json", r.forBundle()},
		{"config.json", cfg},
	}

	if err := writeEntry(z, "README.txt", []byte(readme)); err != nil {
		return err
	}
	for _, e := range entries {
		body, err := json.MarshalIndent(e.body, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", e.name, err)
		}
		if err := writeEntry(z, e.name, append(body, '\n')); err != nil {
			return err
		}
	}
	if err := z.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	return nil
}

func writeEntry(z *zip.Writer, name string, body []byte) error {
	f, err := z.CreateHeader(&zip.FileHeader{
		Name: name, Method: zip.Deflate, Modified: bundleTime,
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// BundleName is the filename a download is offered under.
func BundleName(at time.Time) string {
	return "valmin-support-" + at.UTC().Format("20060102-150405") + ".zip"
}
