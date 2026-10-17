package notify

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// errDestinationURL reports a stored destination URL that no longer parses. It never quotes the
// URL, which is a credential.
var errDestinationURL = errors.New("the destination URL cannot be read")

// DiscordPostURL is a Discord webhook URL that answers with the message it creates, so the
// message can be edited later.
func DiscordPostURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errDestinationURL
	}
	q := u.Query()
	q.Set("wait", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// DiscordEditURL is the address that edits one message a Discord webhook created. A thread the
// webhook posts to stays in the query, where Discord expects it for an edit too.
func DiscordEditURL(raw, messageID string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errDestinationURL
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/messages/" + url.PathEscape(messageID)
	return u.String(), nil
}

// DiscordMessageID is the id of the message Discord's answer describes, or "" when the answer
// carries none. An id is a snowflake, so anything but digits is refused rather than put in a path.
func DiscordMessageID(reply []byte) string {
	var message struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(reply, &message) != nil || message.ID == "" {
		return ""
	}
	if strings.Trim(message.ID, "0123456789") != "" {
		return ""
	}
	return message.ID
}
