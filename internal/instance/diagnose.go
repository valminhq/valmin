package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/runtime"
)

// Severity of a Finding.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Where a Finding's fix happens, for the SPA to link to.
const (
	TargetMods     = "mods"
	TargetSettings = "settings"
)

// Finding is one explained cause of a failed or degraded start. Its text is shown to the
// operator as written.
type Finding struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Fixes    []string `json:"fixes"`
	// Evidence is the log line the finding rests on, as the server or the loader wrote it.
	Evidence string `json:"evidence,omitempty"`
	// Related names what failed only as a consequence of this finding.
	Related []string `json:"related,omitempty"`
	// Target is where the fix happens: TargetMods or TargetSettings.
	Target string `json:"target,omitempty"`
}

// ModRef is an installed, enabled package as Diagnose needs it.
type ModRef struct {
	FullName      string
	ManifestPaths []string
	Deprecated    bool
}

// Evidence is everything Diagnose reads. Each part is recovered from Docker or the data
// directory, so a diagnosis survives a panel restart.
type Evidence struct {
	State      State
	Modded     bool
	MemLimitMB int
	// Container is nil when the instance has no container.
	Container *runtime.Container
	// Boot is nil when the boot's log was not read.
	Boot *BootScan
	// Plugins is nil when the instance has no loader log.
	Plugins *PluginLoad
	Mods    []ModRef
}

// BootScan is what the current boot's console said, read up to the readiness line.
type BootScan struct {
	Ready bool
	// LastLine is the last non-empty line before readiness, bounded.
	LastLine string
}

// ScanBoot reads c's log from its most recent start, ignoring everything after the readiness line.
func ScanBoot(ctx context.Context, rt runtime.Runtime, c *runtime.Container) (BootScan, error) {
	rc, err := rt.Logs(ctx, c.ID, runtime.LogOptions{Since: bootOf(c)})
	if err != nil {
		return BootScan{}, fmt.Errorf("read logs of container %s: %w", c.ID, err)
	}
	defer func() { _ = rc.Close() }()

	var scan BootScan
	patterns := ActivePatterns()
	if err := DemuxLines(rc, func(l Line) {
		if scan.Ready {
			return
		}
		if ev, ok := patterns.Match(l.Text); ok && ev.Kind == EventReady {
			scan.Ready = true
			return
		}
		if t := strings.TrimSpace(l.Text); t != "" {
			scan.LastLine = t
		}
	}); err != nil {
		return BootScan{}, err
	}
	scan.LastLine = bounded(scan.LastLine)
	return scan, nil
}

// Diagnose explains the current boot, most actionable finding first. A healthy server has none.
// It states only what Docker, the console or the loader log observed.
func Diagnose(e *Evidence) []Finding {
	c := e.Container
	// The loader log describes this boot only if it was written after the container started.
	plugins := e.Plugins
	if plugins != nil && c != nil && plugins.ObservedAt.Before(bootOf(c)) {
		plugins = nil
	}

	var out []Finding
	if c != nil && c.OOMKilled {
		out = append(out, oomKilled(e.MemLimitMB))
	}
	out = append(out, pluginFailures(plugins, e.Mods)...)
	if f, ok := unfinishedStart(e, plugins); ok {
		out = append(out, f)
	}
	if e.Modded && plugins == nil && booted(e) {
		out = append(out, loaderNotRun())
	}
	return out
}

// unfinishedStart explains a boot parked in the error state before its readiness line: still
// running past the deadline, or exited for a reason other than memory.
func unfinishedStart(e *Evidence, plugins *PluginLoad) (Finding, bool) {
	c := e.Container
	if c == nil || e.Boot == nil || e.State != StateError || e.Boot.Ready {
		return Finding{}, false
	}
	if c.Running {
		return notReady(), true
	}
	if c.OOMKilled {
		return Finding{}, false
	}
	firstError := ""
	if plugins != nil {
		firstError = plugins.FirstError
	}
	return exitedEarly(e, firstError), true
}

// booted reports whether the server is running past its readiness line.
func booted(e *Evidence) bool {
	c := e.Container
	if c == nil || !c.Running {
		return false
	}
	return e.State == StateRunning || (e.Boot != nil && e.Boot.Ready)
}

const fewerModsFix = "Large worlds and many mods need more memory. If this started after adding mods, try with fewer."

func oomKilled(limitMB int) Finding {
	f := Finding{
		Code: "oom_killed", Severity: SeverityError, Title: "The server ran out of memory",
		Detail: "The host ran out of memory and the kernel stopped the server.",
		Fixes: []string{
			"Free memory on the host, for example by stopping a server you are not using.", fewerModsFix,
		},
	}
	if limitMB > 0 {
		f.Detail = fmt.Sprintf("The server used all %d MB its memory limit allows, so the kernel stopped it.", limitMB)
		f.Fixes = []string{"Raise the memory limit in the server's settings.", fewerModsFix}
		f.Target = TargetSettings
	}
	return f
}

// pluginFailures is one finding per plugin that failed on its own. Plugins skipped because a
// dependency failed are listed under the first one, since the loader does not say which failure
// each skip followed.
func pluginFailures(load *PluginLoad, mods []ModRef) []Finding {
	if load == nil {
		return nil
	}
	var out []Finding
	var skipped []string
	for _, f := range load.Failed {
		if f.Skipped {
			skipped = append(skipped, pluginLabel(f.LoadedPlugin))
			continue
		}
		out = append(out, pluginFailed(f, mods))
	}
	switch {
	case len(skipped) == 0:
	case len(out) > 0:
		out[0].Related = skipped
	default:
		out = append(out, Finding{
			Code: "plugin_failed", Severity: SeverityError,
			Title:    fmt.Sprintf("%d mod(s) were skipped", len(skipped)),
			Detail:   "The mod loader skipped these because a mod they depend on did not load.",
			Fixes:    []string{"Check the Mods screen for a missing or disabled dependency."},
			Evidence: strings.Join(skipped, ", "), Target: TargetMods,
		})
	}
	return out
}

func pluginFailed(f FailedPlugin, mods []ModRef) Finding {
	name := pluginLabel(f.LoadedPlugin)
	fixes := []string{
		"Check the Mods screen for an update. Mods built for an older game version stop loading after a game update.",
		"If there is no update, disable this mod until its author releases one.",
	}
	if mod, ok := modFor(f.Name, mods); ok {
		name = mod.FullName
		if mod.Deprecated {
			fixes = append(fixes, "Its author has marked it deprecated, so a fix is unlikely. Look for a replacement.")
		}
	}
	return Finding{
		Code: "plugin_failed", Severity: SeverityError, Title: name + " did not load",
		Detail:   "The mod loader could not load this mod, so it is not active on this server.",
		Fixes:    fixes,
		Evidence: f.Reason, Target: TargetMods,
	}
}

func pluginLabel(p LoadedPlugin) string {
	return strings.TrimSpace(p.Name + " " + p.Version)
}

// modFor finds the installed package a plugin name belongs to, by the aliases Loaded uses.
func modFor(plugin string, mods []ModRef) (ModRef, bool) {
	name := normalisePluginName(plugin)
	for _, m := range mods {
		if pluginAliases(m.FullName, m.ManifestPaths)[name] {
			return m, true
		}
	}
	return ModRef{}, false
}

// exitedEarly quotes the loader's first error when this boot recorded one, and otherwise the
// console's last line.
func exitedEarly(e *Evidence, firstError string) Finding {
	f := Finding{
		Code: "exited_early", Severity: SeverityError, Title: "The server stopped while starting",
		Detail: fmt.Sprintf("The server process exited with code %d before it was ready for players.",
			e.Container.ExitCode),
		Fixes:    []string{"The quoted line is the last thing the server printed before it stopped."},
		Evidence: e.Boot.LastLine,
	}
	if firstError != "" {
		f.Evidence = firstError
		f.Fixes = []string{"The quoted line is the first error the mod loader recorded during this start."}
	}
	if e.Modded {
		f.Fixes = append(f.Fixes, "To find out whether a mod is the cause, disable the mods and start again. "+
			"If it starts, enable them a few at a time until it fails.")
		f.Target = TargetMods
	}
	return f
}

func notReady() Finding {
	return Finding{
		Code: "not_ready", Severity: SeverityWarning, Title: "The server has not finished starting",
		Detail: "The server is still running but has not reported that it is ready, so the start timed out.",
		Fixes: []string{
			"Watch the console. The first start after a game update may convert the world, " +
				"which can take several minutes. Do not stop the server while it does.",
			"Once the console shows the server is up, check the server again to clear the error.",
		},
	}
}

func loaderNotRun() Finding {
	return Finding{
		Code: "loader_not_run", Severity: SeverityWarning, Title: "Mods are not running",
		Detail: "This server is modded, but the mod loader wrote no log during this start, " +
			"so the server is most likely running without mods.",
		Fixes: []string{
			"Restart the server, then look here again.",
			"If it persists, check on the Mods screen that the mod loader package is installed and enabled.",
		},
		Target: TargetMods,
	}
}
