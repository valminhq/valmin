package instance

import (
	"bytes"
	"encoding/json"
	"time"
)

// EyePackage is the Thunderstore package of ValminEye, the server plugin that reports game
// events as JSON lines in the log.
const EyePackage = "valminhq-ValminEye"

// eyeFormat is the only ValminEye line format the panel reads. A line of another version is
// ignored rather than guessed at.
const eyeFormat = "1"

// EyeEvent is one ValminEye event. Fields is the line's JSON object as decoded, so a field the
// plugin adds later reaches the consumer without a change here.
type EyeEvent struct {
	TS     time.Time
	Name   string
	Fields map[string]any
}

// identity reads the account a join event names, with the character name beside its platform
// id. It reports false for any other event or a join without an id.
func (e EyeEvent) identity() (PlayerIdentity, bool) {
	if e.Name != "join" {
		return PlayerIdentity{}, false
	}
	id, _ := e.Fields["id"].(string)
	if id == "" {
		return PlayerIdentity{}, false
	}
	name, _ := e.Fields["player"].(string)
	return PlayerIdentity{TS: e.TS, PlatformID: id, Name: name}, true
}

// parseEye decodes the JSON object of an EventEye line. Numbers stay json.Number, so a large
// integer keeps every digit.
func parseEye(raw string, ts time.Time) (EyeEvent, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return EyeEvent{}, false
	}
	if v, _ := fields["v"].(json.Number); v.String() != eyeFormat {
		return EyeEvent{}, false
	}
	name, _ := fields["event"].(string)
	if name == "" {
		return EyeEvent{}, false
	}
	return EyeEvent{TS: ts, Name: name, Fields: fields}, true
}
