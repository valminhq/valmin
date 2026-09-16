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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/api/middleware"
	panelcrypto "github.com/valminhq/valmin/internal/crypto"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	ValheimRCONPackage = "Tristan-ValheimRcon"
	DefaultRCONPort    = 2455
	configPath         = "BepInEx/config/org.tristan.rcon.cfg"
	configSection      = "1. Rcon"
	maxCommandBytes    = 1024
)

var (
	ErrUnsupported      = errors.New("RCON command channel is unavailable")
	ErrInvalidState     = errors.New("instance is not running")
	ErrInvalidCommand   = errors.New("invalid command")
	ErrCommandForbidden = errors.New("command is not allowed")
	ErrRateLimited      = errors.New("command rate limit exceeded")
)

var AllowedCommands = []string{"save", "kick", "ban", "unban", "banned", "ping"}

type DialContext func(ctx context.Context, network, address string) (net.Conn, error)

// Manager discovers, configures, and uses an instance's ValheimRcon channel.
type Manager struct {
	DB      *store.DB
	Runtime runtime.Runtime
	Keeper  *panelcrypto.Keeper
	Dial    DialContext
	limit   *middleware.Limiter
}

func NewManager(db *store.DB, rt runtime.Runtime, keeper *panelcrypto.Keeper) *Manager {
	dialer := &net.Dialer{Timeout: requestTimeout}
	return &Manager{
		DB: db, Runtime: rt, Keeper: keeper, Dial: dialer.DialContext,
		limit: middleware.NewLimiter(30, time.Minute, 5),
	}
}

// Available reports whether the instance has the command-channel plugin installed.
func (m *Manager) Available(ctx context.Context, inst *store.Instance) (bool, error) {
	_, installed, err := m.DB.InstanceModVersion(ctx, inst.ID, ValheimRCONPackage)
	return installed, wrapOptional(err, "detect ValheimRcon")
}

// Configure writes secure defaults or adopts an existing plugin configuration.
func (m *Manager) Configure(ctx context.Context, instanceID, dataDir string) error {
	_, installed, err := m.DB.InstanceModVersion(ctx, instanceID, ValheimRCONPackage)
	if err != nil || !installed {
		return wrapOptional(err, "detect ValheimRcon")
	}
	path := filepath.Join(dataDir, "server", filepath.FromSlash(configPath))
	port, password, raw, changed, err := readOrCreateConfig(path)
	if err != nil {
		return err
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create RCON config directory: %w", err)
		}
		if err := fsutil.WriteFileAtomic(path, raw); err != nil {
			return fmt.Errorf("write RCON config: %w", err)
		}
	}
	storedPort, storedEnvelope, ok, err := m.DB.InstanceRCON(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("read stored RCON settings: %w", err)
	}
	if ok && storedPort == port {
		storedPassword, err := m.Keeper.Decrypt(
			panelcrypto.PurposeRCONPassword,
			panelcrypto.RCONPasswordLocation(instanceID),
			storedEnvelope,
		)
		if err == nil && string(storedPassword) == password {
			return nil
		}
	}
	envelope, err := m.Keeper.Encrypt(
		panelcrypto.PurposeRCONPassword,
		panelcrypto.RCONPasswordLocation(instanceID),
		[]byte(password),
	)
	if err != nil {
		return fmt.Errorf("encrypt RCON password: %w", err)
	}
	if err := m.DB.SetInstanceRCON(ctx, instanceID, port, envelope); err != nil {
		return fmt.Errorf("store RCON settings: %w", err)
	}
	return nil
}

func readOrCreateConfig(path string) (port int, password string, raw []byte, changed bool, err error) {
	raw, err = os.ReadFile(path) //nolint:gosec // path is derived from the instance data directory
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

// Send validates and sends one command over a fresh authenticated RCON connection.
func (m *Manager) Send(ctx context.Context, inst *store.Instance, raw string, unrestricted bool) (string, error) {
	command, err := validateCommand(raw, unrestricted)
	if err != nil {
		return "", err
	}
	if inst.State != "running" || inst.ContainerID == nil {
		return "", ErrInvalidState
	}
	if ok, _ := m.limit.Allow(inst.ID); !ok {
		return "", ErrRateLimited
	}
	available, err := m.Available(ctx, inst)
	if err != nil {
		return "", err
	}
	if !available {
		return "", ErrUnsupported
	}
	if err := m.Configure(ctx, inst.ID, inst.DataDir); err != nil {
		return "", err
	}
	port, envelope, _, err := m.DB.InstanceRCON(ctx, inst.ID)
	if err != nil {
		return "", fmt.Errorf("read RCON settings: %w", err)
	}
	password, err := m.Keeper.Decrypt(
		panelcrypto.PurposeRCONPassword,
		panelcrypto.RCONPasswordLocation(inst.ID),
		envelope,
	)
	if err != nil {
		return "", fmt.Errorf("decrypt RCON password: %w", err)
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
	return executeRCON(ctx, conn, string(password), command)
}

func wrapOptional(err error, detail string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", detail, err)
}

func validateCommand(raw string, unrestricted bool) (string, error) {
	command := strings.TrimSpace(raw)
	if command == "" || len(command) > maxCommandBytes || strings.ContainsAny(command, "\r\n\x00") {
		return "", ErrInvalidCommand
	}
	if unrestricted {
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
