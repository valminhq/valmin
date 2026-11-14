package instance

// Watchdog is what the panel does about a running server whose log has gone quiet.
type Watchdog string

const (
	// WatchdogOff leaves a quiet server alone.
	WatchdogOff Watchdog = "off"
	// WatchdogAlert reports a quiet server and leaves it running.
	WatchdogAlert Watchdog = "alert"
	// WatchdogRestart reports a quiet server and restarts it.
	WatchdogRestart Watchdog = "restart"
)

// ParseWatchdog resolves a stored or requested value to its constant.
func ParseWatchdog(s string) (Watchdog, bool) {
	switch w := Watchdog(s); w {
	case WatchdogOff, WatchdogAlert, WatchdogRestart:
		return w, true
	}
	return "", false
}
