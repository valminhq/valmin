package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/runtime"
)

// GameResolvFile is the name, directly under the data root, of the resolv.conf mounted into
// game containers.
const GameResolvFile = "game-resolv.conf"

// GameResolvConf is the host path of the resolv.conf game containers mount, as the Docker
// daemon sees it. It is empty when game.dns is unset and the containers keep Docker's own.
func (c *Config) GameResolvConf() string {
	if len(c.Game.DNS) == 0 {
		return ""
	}
	return filepath.Join(c.Data.HostRoot, GameResolvFile)
}

// GameResolvBinds is the read-only mount of GameResolvConf over a container's
// /etc/resolv.conf, or nothing when game.dns is unset.
func (c *Config) GameResolvBinds() []runtime.Bind {
	return ResolvBinds(c.GameResolvConf())
}

// ResolvBinds mounts the resolv.conf at hostPath read-only over a container's
// /etc/resolv.conf, or mounts nothing for an empty path.
func ResolvBinds(hostPath string) []runtime.Bind {
	if hostPath == "" {
		return nil
	}
	return []runtime.Bind{{HostPath: hostPath, ContainerPath: "/etc/resolv.conf", ReadOnly: true}}
}

// WriteGameResolv writes the resolv.conf for game.dns under the data root, replacing the
// previous one, or does nothing when game.dns is unset. Call it before creating any game
// container: Docker creates a missing bind source as a directory, and an empty one left that
// way is removed here.
func WriteGameResolv(cfg *Config) error {
	if len(cfg.Game.DNS) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Written by valmin from game.dns each time the panel starts.\n")
	for _, addr := range cfg.Game.DNS {
		fmt.Fprintf(&b, "nameserver %s\n", addr)
	}
	path := filepath.Join(cfg.Data.Root, GameResolvFile)
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the directory in place of %s: %w", path, err)
		}
	}
	if err := fsutil.WriteFileAtomic(path, []byte(b.String())); err != nil {
		return fmt.Errorf("write game resolv.conf: %w", err)
	}
	return nil
}
