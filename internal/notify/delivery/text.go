package delivery

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// jobLabel names a job kind the way the panel's job history does: mod_install reads
// "Mod install".
func jobLabel(kind string) string {
	words := strings.TrimSpace(strings.ReplaceAll(kind, "_", " "))
	if words == "" {
		return "A job"
	}
	return upperFirst(words)
}

// asSentence presents a recorded reason as a sentence: capitalised, ending in a full stop.
func asSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = upperFirst(s)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// formatBytes renders a stored byte count as the panel's inbox does: 1024-based, one decimal
// under ten. A value that is not a count is returned as it is.
func formatBytes(raw string) string {
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return raw
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	unit := 0
	for n >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	if n < 10 {
		return fmt.Sprintf("%.1f %s", n, units[unit])
	}
	return fmt.Sprintf("%.0f %s", n, units[unit])
}

// formatDuration renders a stored Go duration in its two largest units, "6 hours" or
// "1 day 3 hours". A value that is not a duration is returned as it is.
func formatDuration(raw string) string {
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return raw
	}
	d = d.Truncate(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	minutes := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return joinUnits(days, "day", hours, "hour")
	case hours > 0:
		return joinUnits(hours, "hour", minutes, "minute")
	default:
		return plural(minutes, "minute")
	}
}

func joinUnits(major int, majorUnit string, minor int, minorUnit string) string {
	if minor == 0 {
		return plural(major, majorUnit)
	}
	return plural(major, majorUnit) + " " + plural(minor, minorUnit)
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// formatTime renders a stored RFC3339 instant for a reader, in UTC since a receiver's own zone
// is not the panel's to know. A value that is not an instant is returned as it is.
func formatTime(raw string) string {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}
