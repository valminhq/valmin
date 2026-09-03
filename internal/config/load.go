package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the config file location, overridable with --config (10 §1).
const DefaultPath = "/etc/valmin/config.yaml"

const envPrefix = "VALMIN_"

// Load resolves configuration in the precedence order fixed by 10 §1, lowest first:
// built-in defaults, config file, environment variable, command-line flag.
//
// getenv is injected so the precedence chain is testable without touching the process
// environment; pass os.Getenv.
func Load(args []string, getenv func(string) string) (*Config, error) {
	cfg := Defaults()

	path, rest, err := configPath(args, getenv)
	if err != nil {
		return nil, err
	}
	if err := applyFile(&cfg, path); err != nil {
		return nil, err
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return nil, err
	}
	if err := applyFlags(&cfg, rest); err != nil {
		return nil, err
	}

	deriveFromRoot(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// configPath extracts --config before the main flag set is built, since the file must be
// read before the flags that override it. It returns the remaining arguments.
func configPath(args []string, getenv func(string) string) (path string, rest []string, err error) {
	path = DefaultPath
	explicit := false
	if v := getenv(envPrefix + "CONFIG"); v != "" {
		path, explicit = v, true
	}

	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 >= len(args) {
				return "", nil, errors.New("--config requires a path")
			}
			path, explicit = args[i+1], true
			i++
		case strings.HasPrefix(a, "--config="), strings.HasPrefix(a, "-config="):
			_, v, _ := strings.Cut(a, "=")
			path, explicit = v, true
		default:
			rest = append(rest, a)
		}
	}

	if explicit {
		if _, statErr := os.Stat(path); statErr != nil {
			return "", nil, fmt.Errorf("config file %s: %w", path, statErr)
		}
	}
	return path, rest, nil
}

// applyFile overlays the YAML file. A missing file at the default path is not an error:
// the shipped deployment is docker compose, which speaks env (10 §1).
func applyFile(cfg *Config, path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, resolved above
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if err := refuseSecrets(raw, path); err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// refuseSecrets rejects a config file carrying a secret. 10 §1.1 makes this a startup
// error rather than a warning: the file is the artefact that ends up in a git repo or a
// support ticket.
func refuseSecrets(raw []byte, path string) error {
	var probe struct {
		DB struct {
			DSN string `yaml:"dsn"`
		} `yaml:"db"`
	}
	// Ignore parse failures here; applyFile reports them with better context.
	_ = yaml.Unmarshal(raw, &probe)

	if dsnHasPassword(probe.DB.DSN) {
		return fmt.Errorf(
			"%s: db.dsn carries a password; use %sDB_DSN or %sDB_DSN_FILE instead (10 §1.1)",
			path, envPrefix, envPrefix)
	}

	var top map[string]yaml.Node
	_ = yaml.Unmarshal(raw, &top)
	for k := range top {
		if strings.HasPrefix(strings.ToUpper(k), envPrefix) {
			return fmt.Errorf(
				"%s: %s is an environment variable and must not appear in the config file (10 §1.1)",
				path, k)
		}
	}
	return nil
}

// dsnHasPassword reports whether a DSN embeds a password, in either the URL or the
// keyword form.
func dsnHasPassword(dsn string) bool {
	if dsn == "" {
		return false
	}
	if strings.Contains(strings.ToLower(dsn), "password=") {
		return true
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return false
	}
	_, set := u.User.Password()
	return set
}

// applyEnv overlays VALMIN_-prefixed environment variables. Every key has an env
// equivalent by construction: the name is derived from the yaml path (10 §1).
//
// Every key also accepts a _FILE suffix, so Docker and Podman secrets work without an
// entrypoint shim. Setting both forms is a config error, not a precedence question.
func applyEnv(cfg *Config, getenv func(string) string) error {
	for _, f := range fields(cfg) {
		direct := getenv(f.env)
		fromFile := getenv(f.env + "_FILE")

		if direct != "" && fromFile != "" {
			return fmt.Errorf("%s and %s_FILE are both set; pick one (10 §1.1)", f.env, f.env)
		}

		value := direct
		if fromFile != "" {
			b, err := os.ReadFile(fromFile) //nolint:gosec // operator-supplied secret path
			if err != nil {
				return fmt.Errorf("read %s_FILE: %w", f.env, err)
			}
			value = strings.TrimSpace(string(b))
		}
		if value == "" {
			continue
		}
		if err := setField(f, value); err != nil {
			return fmt.Errorf("%s: %w", f.env, err)
		}
	}
	return nil
}

// applyFlags overlays command-line flags, the highest precedence level. One flag per
// key, named by its yaml path: --server.listen, --data.host_root.
func applyFlags(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("valmind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("config", DefaultPath, "path to the YAML config file")

	var firstErr error
	for _, f := range fields(cfg) {
		fs.Func(f.path, "see 10 §1.1", func(v string) error {
			if err := setField(f, v); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("--%s: %w", f.path, err)
			}
			return nil
		})
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	return firstErr
}

// deriveFromRoot fills the two defaults that reference data.root, once data.root has
// settled through the whole precedence chain (10 §1.1).
func deriveFromRoot(cfg *Config) {
	if cfg.DB.DSN == "" {
		cfg.DB.DSN = "file:" + filepath.Join(cfg.Data.Root, "panel.db")
	}
	if cfg.Secrets.MasterKeyFile == "" {
		cfg.Secrets.MasterKeyFile = filepath.Join(cfg.Data.Root, "secret.key")
	}
}

// field is one leaf setting: its yaml path, its environment variable name, and a
// settable reference to it.
type field struct {
	path  string
	env   string
	value reflect.Value
}

// fields walks the config tree and returns every leaf. The env name is derived rather
// than tabulated so that a new key cannot silently lack one.
func fields(cfg *Config) []field {
	var out []field
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := range t.NumField() {
			tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			fv := v.Field(i)
			if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(Duration(0)) {
				walk(fv, path)
				continue
			}
			out = append(out, field{
				path:  path,
				env:   envPrefix + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(path)),
				value: fv,
			})
		}
	}
	walk(reflect.ValueOf(cfg).Elem(), "")
	return out
}

// setField parses s into the field's type.
func setField(f field, s string) error {
	if f.value.Type() == reflect.TypeOf(Duration(0)) {
		d, err := ParseDuration(s)
		if err != nil {
			return err
		}
		f.value.Set(reflect.ValueOf(d))
		return nil
	}

	// exhaustive is kept strict globally for job kinds and instance states (C8); a
	// reflect.Kind switch with an erroring default is the exception.
	switch f.value.Kind() { //nolint:exhaustive // default rejects every other kind
	case reflect.String:
		f.value.SetString(s)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("want an integer, got %q: %w", s, err)
		}
		f.value.SetInt(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("want a boolean, got %q: %w", s, err)
		}
		f.value.SetBool(b)
	case reflect.Slice:
		parts := strings.Split(s, ",")
		trimmed := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				trimmed = append(trimmed, p)
			}
		}
		f.value.Set(reflect.ValueOf(trimmed))
	default:
		return fmt.Errorf("unsupported config kind %s", f.value.Kind())
	}
	return nil
}

// Validate rejects a configuration the panel cannot honour. 01 §6: fail visibly, and
// refuse to start rather than degrade into a panel that half works. Every problem is
// reported, not just the first.
func Validate(cfg *Config) error {
	checks := []func(*Config) []error{
		validatePaths,
		validateGame,
		validateNetwork,
		validateStore,
		validateObservability,
	}
	errs := make([]error, 0, len(checks))
	for _, check := range checks {
		errs = append(errs, check(cfg)...)
	}
	return errors.Join(errs...)
}

// collect adapts a sequence of checks into an error slice.
type collector []error

func (c *collector) failf(format string, a ...any) { *c = append(*c, fmt.Errorf(format, a...)) }

func validatePaths(cfg *Config) []error {
	var c collector
	if cfg.Server.ExternalURL == "" {
		c.failf("server.external_url is required; invite URLs are handed to humans (10 §1.1)")
	} else if u, err := url.Parse(cfg.Server.ExternalURL); err != nil || !u.IsAbs() {
		c.failf("server.external_url must be an absolute URL, got %q", cfg.Server.ExternalURL)
	} else {
		warnIfCookiesCannotBeStored(u)
	}

	if cfg.Data.HostRoot == "" {
		c.failf("data.host_root is required: the path as the host sees it, not as this " +
			"container sees it (02 §5, 10 §1.2)")
	} else if !filepath.IsAbs(cfg.Data.HostRoot) {
		c.failf("data.host_root must be an absolute path, got %q", cfg.Data.HostRoot)
	}
	if !filepath.IsAbs(cfg.Data.Root) {
		c.failf("data.root must be an absolute path, got %q", cfg.Data.Root)
	}
	if cfg.Data.FreeSpaceFloorBytes < 0 {
		c.failf("data.free_space_floor_bytes must not be negative, got %d",
			cfg.Data.FreeSpaceFloorBytes)
	}
	return c
}

func validateGame(cfg *Config) []error {
	var c collector
	// ADR-008: the floor exists to stop Docker escalating to SIGKILL mid-write. The
	// measured 3-5 s stop is shutdown overhead, not flush time.
	if cfg.Game.StopTimeout.Std() < MinStopTimeout {
		c.failf("game.stop_timeout is %s but the floor is %s (ADR-008, 03 §3.2.1)",
			cfg.Game.StopTimeout, MinStopTimeout)
	}
	if cfg.Game.DefaultMemMB <= 0 {
		c.failf("game.default_mem_mb must be positive (03 §3.3)")
	}
	return c
}

func validateNetwork(cfg *Config) []error {
	var c collector
	// 03 §2: a server occupies the game port and game port + 1.
	if cfg.Ports.Base < 1024 || cfg.Ports.Base > 65535 {
		c.failf("ports.base must be between 1024 and 65535, got %d", cfg.Ports.Base)
	}
	if cfg.Ports.Stride < 2 {
		c.failf("ports.stride must be at least 2: an instance occupies two consecutive "+
			"UDP ports (03 §2), got %d", cfg.Ports.Stride)
	}
	for _, cidr := range cfg.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			c.failf("server.trusted_proxies: %q is not a CIDR prefix (10 §5)", cidr)
		}
	}
	return c
}

func validateStore(cfg *Config) []error {
	var c collector
	if cfg.DB.Driver != "sqlite" && cfg.DB.Driver != "postgres" {
		c.failf("db.driver must be sqlite or postgres, got %q", cfg.DB.Driver)
	}
	if strings.HasPrefix(cfg.DB.DSN, "file:") && dsnHasPassword(cfg.DB.DSN) {
		c.failf("db.dsn for sqlite must not carry a password")
	}
	return c
}

func validateObservability(cfg *Config) []error {
	var c collector
	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		c.failf("log.level must be debug, info, warn or error, got %q", cfg.Log.Level)
	}
	switch cfg.Log.Format {
	case "json", "text":
	default:
		c.failf("log.format must be json or text, got %q", cfg.Log.Format)
	}
	if cfg.Jobs.ReadySettle.Std() >= cfg.Jobs.ReadyTimeout.Std() {
		c.failf("jobs.ready_settle (%s) must be shorter than jobs.ready_timeout (%s) (12 §3.3)",
			cfg.Jobs.ReadySettle, cfg.Jobs.ReadyTimeout)
	}
	if cfg.Jobs.LeaseTTL.Std() <= 0 {
		c.failf("jobs.lease_ttl must be positive (12 §5)")
	}
	return c
}

// warnIfCookiesCannotBeStored is a startup warning for the one misconfiguration whose
// symptom is a successful login that does nothing.
//
// The session and CSRF cookies are `Secure` unconditionally (10 §4.1, 11 §6.2). A browser
// will not store a `Secure` cookie received over plain `http://`, except from `localhost`,
// which every major browser treats as trustworthy. So on `http://<lan-ip>` the login
// returns 200 and a user, the SPA believes it is signed in, and every request after it is
// 401 because the cookie never reached the server.
//
// Nothing server-side can detect this — from here the login worked — and it is warned about
// rather than refused because a reverse proxy terminating TLS in front of the panel is a
// legitimate deployment, and this value is what the browser sees. It is easily mistaken for
// the origin check, which fails loudly and is not the cause.
func warnIfCookiesCannotBeStored(u *url.URL) {
	if u.Scheme != "http" {
		return
	}
	if host := u.Hostname(); host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return
	}
	slog.Warn(
		"server.external_url is plain http on a non-localhost host, "+
			"so browsers will refuse to store this panel's cookies",
		slog.String("external_url", u.String()),
		slog.String("symptom", "login appears to succeed and every request after it is 401"),
		slog.String("fix", "serve the panel over https, or reach it as http://localhost via an ssh tunnel"))
}
