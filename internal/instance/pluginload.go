package instance

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// bepinexLog is BepInEx's own log file, relative to an instance's data directory. Load
// verification reads it rather than the container's stdout, which BepInEx reaches only while
// `[Logging.Console] Enabled` is true (03 §5.5) — the setting whose absence this feature exists
// to notice.
const bepinexLog = "server/BepInEx/LogOutput.log"

// LoadedPlugin is one plugin BepInEx's chainloader named, from `Loading [Name Version]`.
type LoadedPlugin struct {
	Name    string
	Version string
}

// PluginLoad is what the most recent chainloader run in the log said. The most recent
// one, not the whole file: whether BepInEx truncates its log per boot or appends to it has
// not been measured, and taking everything after the last count line is correct either way.
type PluginLoad struct {
	// Declared is the number from `(\d+) plugins? to load`, or -1 if the run named none.
	// 03 §5.3 prefers *counting* the per-plugin lines and using this as a cross-check, so
	// it is kept beside them rather than trusted over them.
	Declared int
	Plugins  []LoadedPlugin
	// ObservedAt is when BepInEx last wrote to the log, which is the run these results come
	// from. The lines themselves carry no timestamp (03 §5.3's captures), so this is the
	// file's own — near enough to say which boot, and not presented as more than that.
	ObservedAt time.Time
}

// ReadPluginLoad parses the chainloader run in an instance's BepInEx log. A nil result with a
// nil error is a server with no such log, never started since BepInEx was installed or never
// modded: an absence of information rather than a failure to report.
func ReadPluginLoad(dataDir string) (*PluginLoad, error) {
	p := filepath.Join(dataDir, filepath.FromSlash(bepinexLog))
	f, err := os.Open(p) //nolint:gosec // dataDir is the panel's own layout, not a user string
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()

	load := parsePluginLoad(f)
	if info, err := f.Stat(); err == nil {
		load.ObservedAt = info.ModTime()
	}
	return &load, nil
}

// parsePluginLoad reads the log a line at a time, keeping only the last run's state: the file
// holds a server's whole mod output and can be large. Matching goes through the one shared
// pattern set, so no literal is minted here (14 §4.5, E9).
func parsePluginLoad(r io.Reader) PluginLoad {
	load := PluginLoad{Declared: -1}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			ev, matched := DefaultPatterns.Match(strings.TrimRight(line, "\r\n"))
			switch {
			case !matched:
			case ev.Kind == EventPluginCount:
				// A count line begins a run: everything before it belongs to an earlier
				// one, whether that is a previous boot in an appended file or BepInEx's
				// own second preloader pass (02 §4.5).
				n, convErr := strconv.Atoi(ev.Groups[1])
				if convErr != nil {
					n = -1
				}
				load = PluginLoad{Declared: n}
			case ev.Kind == EventPluginLoading:
				load.Plugins = append(load.Plugins, parseLoadedPlugin(ev.Groups[1]))
			}
		}
		if err != nil {
			return load
		}
	}
}

// parseLoadedPlugin splits `Jotunn 2.29.2` into its name and version. A plugin name can
// contain spaces, so the version is taken from the last one; a bracket with no space at all
// is all name.
func parseLoadedPlugin(inner string) LoadedPlugin {
	inner = strings.TrimSpace(inner)
	if i := strings.LastIndex(inner, " "); i > 0 {
		return LoadedPlugin{Name: inner[:i], Version: inner[i+1:]}
	}
	return LoadedPlugin{Name: inner}
}

// Discrepancy reports the count line and the per-plugin lines disagreeing, as a sentence, or ""
// when they agree or there was no count line. It is reported and never resolved: picking a
// winner would hide a plugin BepInEx meant to load and never named (03 §5.3).
func (l *PluginLoad) Discrepancy() string {
	if l == nil || l.Declared < 0 || l.Declared == len(l.Plugins) {
		return ""
	}
	return fmt.Sprintf("BepInEx said %d plugin(s) to load and named %d",
		l.Declared, len(l.Plugins))
}

// Loaded reports whether this run named a plugin belonging to an installed package.
//
// The match is a heuristic: a `Loading [...]` line carries the assembly's plugin name, which
// nothing binds to the package's own. A package is matched by the name half of its full name and
// by the base names of the `.dll` files its manifest placed, which is all the panel knows. A miss
// reads as `not_seen` rather than as a broken mod.
func (l *PluginLoad) Loaded(fullName string, manifestPaths []string) bool {
	if l == nil {
		return false
	}
	aliases := pluginAliases(fullName, manifestPaths)
	for _, p := range l.Plugins {
		if aliases[normalisePluginName(p.Name)] {
			return true
		}
	}
	return false
}

// IsPlugin reports whether a package places anything BepInEx would load. A framework package's
// assemblies live under `BepInEx/core/` and a config-only package ships none, so neither is ever
// named by a `Loading [...]` line and neither has a load status to report.
func IsPlugin(manifestPaths []string) bool {
	for _, p := range manifestPaths {
		if strings.HasPrefix(p, pluginRoot) && strings.EqualFold(path.Ext(p), ".dll") {
			return true
		}
	}
	return false
}

const pluginRoot = "BepInEx/plugins/"

func pluginAliases(fullName string, manifestPaths []string) map[string]bool {
	aliases := map[string]bool{}
	// The name half of Namespace-Name: the namespace is everything before the first dash
	// (03 §6.2), so what remains is the package's own name.
	if _, name, ok := strings.Cut(fullName, "-"); ok && name != "" {
		aliases[normalisePluginName(name)] = true
	}
	for _, p := range manifestPaths {
		if !strings.HasPrefix(p, pluginRoot) || !strings.EqualFold(path.Ext(p), ".dll") {
			continue
		}
		aliases[normalisePluginName(strings.TrimSuffix(path.Base(p), path.Ext(p)))] = true
	}
	return aliases
}

// normalisePluginName drops case and the separators packages and assemblies spell differently,
// such as `AAA_Crafting` against `AAACrafting`. Deliberately not a fuzzy match: two names
// differing by an actual character stay different.
func normalisePluginName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
