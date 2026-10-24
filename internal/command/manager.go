package command

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/ratelimit"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	// ValheimRCONPackage is the plugin that provides the command channel.
	ValheimRCONPackage = "Tristan-ValheimRcon"
	// DefaultRCONPort is the port the panel writes into a new plugin config.
	DefaultRCONPort = 2455
	// ConfigFile is the plugin's config file in BepInEx/config. It holds the instance's RCON
	// password, so no copy of one server's tree may carry it to another.
	ConfigFile = "org.tristan.rcon.cfg"
	// ConfigPath is ConfigFile relative to an instance's server directory.
	ConfigPath      = "BepInEx/config/" + ConfigFile
	configSection   = "1. Rcon"
	maxCommandBytes = 1024
)

var (
	ErrUnsupported      = errors.New("RCON command channel is unavailable")
	ErrInvalidState     = errors.New("instance is not running")
	ErrInvalidCommand   = errors.New("invalid command")
	ErrCommandForbidden = errors.New("command is not allowed")
	ErrRateLimited      = errors.New("command rate limit exceeded")
)

// AllowedCommands are the commands a person without commands.unrestricted may send, matched by
// first word.
var AllowedCommands = []string{"save", "kick", "ban", "unban", "banned", "ping"}

// DialContext opens the connection to a server's RCON port.
type DialContext func(ctx context.Context, network, address string) (net.Conn, error)

// Scope is which commands a person may send.
type Scope int

const (
	// ScopeOperator allows only AllowedCommands.
	ScopeOperator Scope = iota + 1
	// ScopeAny allows any command.
	ScopeAny
)

// Manager discovers, configures, and uses an instance's ValheimRcon channel. Build it with
// NewManager.
type Manager struct {
	DB      *store.DB
	Runtime runtime.Runtime
	Dial    DialContext
	// people and panel are separate per-server budgets, so console use cannot delay or fail
	// the panel's own warnings and world-tool commands.
	people *ratelimit.Limiter
	panel  *ratelimit.Limiter
}

// NewManager builds a Manager with its rate limits.
func NewManager(db *store.DB, rt runtime.Runtime) *Manager {
	dialer := &net.Dialer{Timeout: requestTimeout}
	return &Manager{
		DB: db, Runtime: rt, Dial: dialer.DialContext,
		people: ratelimit.New(30, time.Minute, 5),
		panel:  ratelimit.New(60, time.Minute, 20),
	}
}

// Available reports whether the instance has the command-channel plugin installed and enabled.
func (m *Manager) Available(ctx context.Context, inst *store.Instance) (bool, error) {
	_, installed, err := m.DB.EnabledModVersion(ctx, inst.ID, ValheimRCONPackage)
	return installed, wrapOptional(err, "detect ValheimRcon")
}

// Configure writes secure defaults or adopts an existing plugin configuration. It is a no-op
// for an instance without the plugin.
func (m *Manager) Configure(ctx context.Context, instanceID, dataDir string) error {
	_, installed, err := m.DB.EnabledModVersion(ctx, instanceID, ValheimRCONPackage)
	if err != nil || !installed {
		return wrapOptional(err, "detect ValheimRcon")
	}
	_, _, err = settings(dataDir)
	return err
}

// settings reads the plugin's port and password, writing secure defaults for whatever the
// file does not set.
func settings(dataDir string) (port int, password string, err error) {
	// The server tree is writable by the game, so the file is reached through a root: a
	// symlink planted there cannot lead to another server's config.
	root, err := os.OpenRoot(filepath.Join(dataDir, "server"))
	if err != nil {
		return 0, "", fmt.Errorf("open server directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	port, password, raw, changed, err := readOrCreateConfig(root)
	if err != nil {
		return 0, "", err
	}
	if changed {
		if err := root.MkdirAll(path.Dir(ConfigPath), 0o750); err != nil {
			return 0, "", fmt.Errorf("create RCON config directory: %w", err)
		}
		if err := fsutil.WriteFileAtomicIn(root, ConfigPath, raw); err != nil {
			return 0, "", fmt.Errorf("write RCON config: %w", err)
		}
	}
	return port, password, nil
}

// ClearPassword blanks the RCON password in the plugin config under serverDir, if there is one,
// so the next Configure mints a fresh password. A copied server tree uses it to stop sharing its
// source's password.
func ClearPassword(serverDir string) error {
	root, err := os.OpenRoot(serverDir)
	if err != nil {
		return fmt.Errorf("open server directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat RCON config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("RCON config %s is not a regular file", ConfigPath)
	}
	raw, err := root.ReadFile(ConfigPath)
	if err != nil {
		return fmt.Errorf("read RCON config: %w", err)
	}
	doc := modconfig.Parse(raw)
	if _, ok := doc.Get(configSection, "Password"); !ok {
		return nil
	}
	if err := doc.Set(configSection, "Password", ""); err != nil {
		return fmt.Errorf("clear RCON config password: %w", err)
	}
	if err := fsutil.WriteFileAtomicIn(root, ConfigPath, doc.Bytes()); err != nil {
		return fmt.Errorf("write RCON config: %w", err)
	}
	return nil
}

// readOrCreateConfig reads the plugin config inside root, or composes a new one. A config that
// is not a regular file is refused.
func readOrCreateConfig(root *os.Root) (port int, password string, raw []byte, changed bool, err error) {
	info, err := root.Lstat(ConfigPath)
	if err == nil && !info.Mode().IsRegular() {
		return 0, "", nil, false, fmt.Errorf("RCON config %s is not a regular file", ConfigPath)
	}
	if err == nil {
		raw, err = root.ReadFile(ConfigPath)
	}
	if errors.Is(err, os.ErrNotExist) {
		password, err = newPassword()
		if err != nil {
			return 0, "", nil, false, err
		}
		raw = fmt.Appendf(nil, "[%s]\nPort = %d\nPassword = %s\nWhitelist IP mask =\nBlacklist IP mask =\n",
			configSection, DefaultRCONPort, password)
		return DefaultRCONPort, password, raw, true, nil
	}
	if err != nil {
		return 0, "", nil, false, fmt.Errorf("read RCON config: %w", err)
	}

	doc := modconfig.Parse(raw)
	if value, ok := doc.Get(configSection, "Port"); ok {
		port, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > 65535 {
			return 0, "", nil, false, fmt.Errorf("RCON config has invalid port %q", value)
		}
	} else {
		// The document edits settings, never adds them (ADR-010), and the plugin's own
		// default port is not DefaultRCONPort — assuming either number would dial a port
		// nothing listens on.
		return 0, "", nil, false, fmt.Errorf("RCON config has no Port in [%s]", configSection)
	}
	password, _ = doc.Get(configSection, "Password")
	password = strings.TrimSpace(password)
	if password == "" {
		password, err = newPassword()
		if err != nil {
			return 0, "", nil, false, err
		}
		if err := doc.Set(configSection, "Password", password); err != nil {
			return 0, "", nil, false, fmt.Errorf("set RCON config password: %w", err)
		}
		changed = true
		raw = doc.Bytes()
	}
	return port, password, raw, changed, nil
}

func newPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate RCON password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Send sends one command on the panel's own behalf, such as a warning or a world tool's
// command. Any command is allowed, and the panel's sends have their own rate budget.
func (m *Manager) Send(ctx context.Context, inst *store.Instance, raw string) (string, error) {
	return m.send(ctx, inst, raw, ScopeAny, m.panel)
}

// SendAs sends one command on a person's behalf, held to scope and to the people's budget.
func (m *Manager) SendAs(ctx context.Context, inst *store.Instance, raw string, scope Scope) (string, error) {
	return m.send(ctx, inst, raw, scope, m.people)
}

// send validates and sends one command over a fresh authenticated RCON connection.
func (m *Manager) send(
	ctx context.Context, inst *store.Instance, raw string, scope Scope, limit *ratelimit.Limiter,
) (string, error) {
	command, err := Validate(raw, scope)
	if err != nil {
		return "", err
	}
	if inst.State != "running" || inst.ContainerID == nil {
		return "", ErrInvalidState
	}
	if ok, _ := limit.Allow(inst.ID); !ok {
		return "", ErrRateLimited
	}
	available, err := m.Available(ctx, inst)
	if err != nil {
		return "", err
	}
	if !available {
		return "", ErrUnsupported
	}
	port, password, err := settings(inst.DataDir)
	if err != nil {
		return "", err
	}
	container, err := m.Runtime.Inspect(ctx, *inst.ContainerID)
	if err != nil {
		return "", fmt.Errorf("inspect RCON container: %w", err)
	}
	if !container.Running || len(container.NetworkAddresses) == 0 {
		return "", ErrInvalidState
	}
	address, err := rconAddress(container.NetworkAddresses[0], port)
	if err != nil {
		return "", err
	}
	conn, err := m.Dial(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("connect to RCON: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return executeRCON(ctx, conn, password, command)
}

func wrapOptional(err error, detail string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", detail, err)
}

// Validate returns the trimmed command when it is one line, within the size limit and allowed
// under scope.
func Validate(raw string, scope Scope) (string, error) {
	command := strings.TrimSpace(raw)
	if command == "" || len(command) > maxCommandBytes || strings.ContainsAny(command, "\r\n\x00") {
		return "", ErrInvalidCommand
	}
	if scope == ScopeAny {
		return command, nil
	}
	name, _, _ := strings.Cut(command, " ")
	for _, allowed := range AllowedCommands {
		if strings.EqualFold(name, allowed) {
			return command, nil
		}
	}
	return "", ErrCommandForbidden
}

func rconAddress(raw string, port int) (string, error) {
	address, err := netip.ParseAddr(raw)
	if err != nil || address.IsUnspecified() {
		return "", fmt.Errorf("invalid RCON container address %q", raw)
	}
	return net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
}
