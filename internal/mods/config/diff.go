package config

import "strings"

// Change is one setting that differs between two documents. Key is "Section.Key", or the bare
// key for a setting that precedes any section. From is nil for an added setting and To for a
// removed one. A secret setting carries neither: only the fact that it changed is reported.
type Change struct {
	Key    string
	From   *string
	To     *string
	Secret bool
}

// secretMarkers are the fragments of a key name that mark its value as a credential.
var secretMarkers = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "webhook", "credential",
}

// secretKey reports whether a setting's name suggests its value must not be recorded.
func secretKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range secretMarkers {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// Diff lists the settings in effect that differ between two files: changed and removed ones in
// the order of before, then added ones in the order of after. A difference outside a value
// (comments, spacing, line endings) is not a change.
func Diff(before, after []byte) []Change {
	was, is := Parse(before), Parse(after)
	var out []Change
	for _, s := range was.Settings() {
		now, ok := is.Get(s.Section, s.Key)
		switch {
		case !ok:
			out = append(out, newChange(s.Section, s.Key, &s.Value, nil))
		case now != s.Value:
			out = append(out, newChange(s.Section, s.Key, &s.Value, &now))
		}
	}
	for _, s := range is.Settings() {
		if _, ok := was.Get(s.Section, s.Key); !ok {
			out = append(out, newChange(s.Section, s.Key, nil, &s.Value))
		}
	}
	return out
}

func newChange(section, key string, from, to *string) Change {
	c := Change{Key: key, From: from, To: to}
	if section != "" {
		c.Key = section + "." + key
	}
	if secretKey(key) {
		c.From, c.To, c.Secret = nil, nil, true
	}
	return c
}
