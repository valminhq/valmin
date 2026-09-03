package config

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Config is the operator-owned settings tree of 10 §1.1. Anything an operator must set
// before the panel can start lives here; anything set through the UI afterwards lives in
// the database. Nothing is in both.
type Config struct {
	Server       Server       `yaml:"server"`
	Jobs         Jobs         `yaml:"jobs"`
	Data         Data         `yaml:"data"`
	DB           DB           `yaml:"db"`
	Docker       Docker       `yaml:"docker"`
	Secrets      Secrets      `yaml:"secrets"`
	Game         Game         `yaml:"game"`
	Ports        Ports        `yaml:"ports"`
	Thunderstore Thunderstore `yaml:"thunderstore"`
	Auth         Auth         `yaml:"auth"`
	Log          Log          `yaml:"log"`
}

type Server struct {
	Listen string `yaml:"listen"`
	// TrustedProxies is a CIDR allowlist, empty by default. Empty means the socket peer
	// address is used verbatim and X-Forwarded-For is ignored (10 §5).
	TrustedProxies []string `yaml:"trusted_proxies"`
	// RequestTimeout applies to /api/v1 handlers only. There is never a server-wide
	// WriteTimeout: it severs the console WebSocket (11 §8.1).
	RequestTimeout Duration `yaml:"request_timeout"`
	BodyLimitBytes int64    `yaml:"body_limit_bytes"`
	ShutdownGrace  Duration `yaml:"shutdown_grace"`
	// ExternalURL is required. Invite URLs are handed to humans (04 §3).
	ExternalURL string `yaml:"external_url"`
}

type Jobs struct {
	LeaseTTL         Duration `yaml:"lease_ttl"`
	ReadyTimeout     Duration `yaml:"ready_timeout"`
	ReadySettle      Duration `yaml:"ready_settle"`
	ProgressInterval Duration `yaml:"progress_interval"`
	LogCap           int      `yaml:"log_cap"`
	RetentionDays    int      `yaml:"retention_days"`
}

type Data struct {
	// Root is the path as the panel container sees it.
	Root string `yaml:"root"`
	// HostRoot is the path as the host sees it, and is required. It is verified at
	// startup by a token round-trip through a throwaway container, not trusted (10 §1.2).
	HostRoot string `yaml:"host_root"`
	// FreeSpaceFloorBytes is the free space Root must have for the panel to start. It
	// clears Valheim's own ~6.4 MB silent save-stop threshold by three orders of
	// magnitude and covers one game install (10 §2, 03 §3.4).
	FreeSpaceFloorBytes int64 `yaml:"free_space_floor_bytes"`
}

type DB struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type Docker struct {
	Endpoint string `yaml:"endpoint"`
	// APIVersion is negotiated when empty. Pin only if negotiation misbehaves.
	APIVersion string `yaml:"api_version"`
}

type Secrets struct {
	MasterKeyFile string `yaml:"master_key_file"`
}

type Game struct {
	// Image is digest-pinned in production (08 §5).
	Image string `yaml:"image"`
	// StopTimeout is a floor, not a suggestion. Below MinStopTimeout, Docker escalates
	// to SIGKILL mid-write (ADR-008, 03 §3.2.1).
	StopTimeout  Duration `yaml:"stop_timeout"`
	DefaultMemMB int      `yaml:"default_mem_mb"`
	// SteamCMDImage is the throwaway container 08 §3.2 runs to install the dedicated
	// server (896660). Not fixed by the pack — ADR-064 records the choice.
	SteamCMDImage string `yaml:"steamcmd_image"`
}

type Ports struct {
	Base   int `yaml:"base"`
	Stride int `yaml:"stride"`
}

type Thunderstore struct {
	BaseURL      string   `yaml:"base_url"`
	SyncInterval Duration `yaml:"sync_interval"`
}

type Auth struct {
	SessionIdleTTL     Duration `yaml:"session_idle_ttl"`
	SessionAbsoluteTTL Duration `yaml:"session_absolute_ttl"`
	InviteTTL          Duration `yaml:"invite_ttl"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Logger builds the daemon's structured logger from log.level and log.format. Validate
// has already rejected any other value.
func (l Log) Logger(w io.Writer) *slog.Logger {
	var level slog.Level
	switch l.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if l.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// MinStopTimeout is the floor from ADR-008. Stops have been measured at 3-5 s, but that
// is shutdown overhead rather than flush time and does not license lowering it.
const MinStopTimeout = 120 * time.Second

// Defaults returns the built-in defaults of 10 §1.1, the lowest precedence level.
// DB.DSN and Secrets.MasterKeyFile derive from Data.Root and are filled by Load once
// Data.Root has settled.
func Defaults() Config {
	return Config{
		Server: Server{
			Listen:         ":8080",
			TrustedProxies: nil,
			RequestTimeout: Duration(30 * time.Second),
			BodyLimitBytes: 1 << 20,
			ShutdownGrace:  Duration(60 * time.Second),
		},
		Jobs: Jobs{
			LeaseTTL:         Duration(30 * time.Second),
			ReadyTimeout:     Duration(180 * time.Second),
			ReadySettle:      Duration(15 * time.Second),
			ProgressInterval: Duration(2 * time.Second),
			LogCap:           64 << 10,
			RetentionDays:    30,
		},
		Data:   Data{Root: "/srv/valmin", FreeSpaceFloorBytes: 2 << 30},
		DB:     DB{Driver: "sqlite"},
		Docker: Docker{Endpoint: "unix:///var/run/docker.sock"},
		Game: Game{
			Image:         "valmin/valheim:dev",
			StopTimeout:   Duration(MinStopTimeout),
			DefaultMemMB:  4096,
			SteamCMDImage: "steamcmd/steamcmd:latest",
		},
		Ports: Ports{Base: 2456, Stride: 5},
		Thunderstore: Thunderstore{
			BaseURL:      "https://thunderstore.io",
			SyncInterval: Duration(time.Hour),
		},
		Auth: Auth{
			SessionIdleTTL:     Duration(24 * time.Hour),
			SessionAbsoluteTTL: Duration(30 * 24 * time.Hour),
			InviteTTL:          Duration(168 * time.Hour),
		},
		Log: Log{Level: "info", Format: "json"},
	}
}

// Duration is a time.Duration that also accepts a whole number of days, since 10 §1.1
// writes auth.session_absolute_ttl as 30d and time.ParseDuration has no day unit.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil {
			return Duration(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return Duration(v), nil
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("duration must be a string such as 30s, 24h or 30d: %w", err)
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
