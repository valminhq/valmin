package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Steam news for the game itself. The news feed belongs to the game's client app, not to the
// dedicated server app the panel installs, and a patch post is tagged "patchnotes".
const (
	steamNewsBaseURL   = "https://api.steampowered.com"
	steamNewsClientApp = "892970"
	steamNewsTag       = "patchnotes"
	// steamNewsScanned is how many recent posts are fetched to find patch notes among them.
	steamNewsScanned = 30
	// patchNotesKept is how many patch posts are returned.
	patchNotesKept     = 5
	patchNotesTTL      = time.Hour
	patchNotesRetry    = 5 * time.Minute
	steamNewsBodyLimit = 4 << 20
)

// PatchNote is one patch post, with its text reduced to plain lines.
type PatchNote struct {
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Text        string    `json:"text"`
}

// PatchNotesFeed reads the game's recent patch notes from Steam and keeps them for an hour.
// The zero value is not usable; build one with NewPatchNotesFeed.
type PatchNotesFeed struct {
	client  *http.Client
	baseURL string
	now     func() time.Time

	mu        sync.Mutex
	fetchedAt time.Time
	notes     []PatchNote
	// failure is the last failed fetch made with nothing cached, and failedAt when it happened.
	failure  error
	failedAt time.Time
}

// NewPatchNotesFeed returns a feed that asks Steam through client.
func NewPatchNotesFeed(client *http.Client) *PatchNotesFeed {
	return &PatchNotesFeed{client: client, baseURL: steamNewsBaseURL, now: time.Now}
}

// Notes returns the newest patch notes first. A failed refresh returns the notes fetched
// before, and an error only when there are none. Steam is asked again after patchNotesRetry
// following a failure, not on every request meanwhile. A request that was cancelled is not a
// failure of Steam and is not remembered.
func (f *PatchNotesFeed) Notes(ctx context.Context) ([]PatchNote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notes != nil && f.now().Sub(f.fetchedAt) < patchNotesTTL {
		return f.notes, nil
	}
	if f.notes == nil && f.failure != nil && f.now().Sub(f.failedAt) < patchNotesRetry {
		return nil, f.failure
	}
	notes, err := f.fetch(ctx)
	if err != nil {
		if f.notes != nil {
			f.fetchedAt = f.now().Add(patchNotesRetry - patchNotesTTL)
			return f.notes, nil
		}
		if ctx.Err() == nil {
			f.failure, f.failedAt = err, f.now()
		}
		return nil, err
	}
	f.notes, f.fetchedAt = notes, f.now()
	return notes, nil
}

// steamNewsResponse is the part of GetNewsForApp's answer the feed reads.
type steamNewsResponse struct {
	AppNews struct {
		NewsItems []struct {
			GID      string   `json:"gid"`
			Title    string   `json:"title"`
			Contents string   `json:"contents"`
			Date     int64    `json:"date"`
			Tags     []string `json:"tags"`
		} `json:"newsitems"`
	} `json:"appnews"`
}

func (f *PatchNotesFeed) fetch(ctx context.Context) ([]PatchNote, error) {
	q := url.Values{
		"appid": {steamNewsClientApp}, "count": {fmt.Sprint(steamNewsScanned)}, "maxlength": {"0"},
		"feeds": {"steam_community_announcements"}, "format": {"json"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.baseURL+"/ISteamNews/GetNewsForApp/v2/?"+q.Encode(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build the Steam news request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ask Steam for the game's news: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the news feed answered %s", resp.Status)
	}
	var body steamNewsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, steamNewsBodyLimit)).Decode(&body); err != nil {
		return nil, fmt.Errorf("read Steam's news: %w", err)
	}
	return patchNotesFrom(&body), nil
}

// patchNotesFrom keeps the posts tagged as patch notes, in the order Steam lists them. The
// result is never nil, so an empty feed is told apart from one not fetched yet.
func patchNotesFrom(body *steamNewsResponse) []PatchNote {
	notes := []PatchNote{}
	for _, item := range body.AppNews.NewsItems {
		if len(notes) == patchNotesKept {
			break
		}
		isPatch := false
		for _, tag := range item.Tags {
			isPatch = isPatch || tag == steamNewsTag
		}
		if !isPatch {
			continue
		}
		notes = append(notes, PatchNote{
			Title:       item.Title,
			URL:         "https://store.steampowered.com/news/app/" + steamNewsClientApp + "/view/" + item.GID,
			PublishedAt: time.Unix(item.Date, 0).UTC(),
			Text:        plainNewsText(item.Contents),
		})
	}
	return notes
}

var (
	// newsEmbeds are tags whose content is media or a link target, not reading text.
	newsEmbeds = regexp.MustCompile(
		`(?is)\[(img|previewyoutube|dynamiclink)[^\]]*\].*?\[/(img|previewyoutube|dynamiclink)\]`)
	newsItem = regexp.MustCompile(`\[\*\]\s*`)
	// newsHead are the closing tags after which the text starts a new line.
	newsHead = regexp.MustCompile(`(?i)\[/(h[1-6]|p|list|olist|quote|tr|th|td)\]`)
	newsTag  = regexp.MustCompile(
		`(?i)\[/?(b|i|u|s|strike|h[1-6]|p|url|list|olist|quote|spoiler|noparse|code|table|tr|th|td|hr)(=[^\]]*)?\]`)
	newsBlank = regexp.MustCompile(`\n{3,}`)
)

// plainNewsText turns Steam's markup into plain lines: embedded images and videos are dropped,
// list items become bullets and every other tag is removed with its text kept.
func plainNewsText(markup string) string {
	text := strings.ReplaceAll(markup, "\r\n", "\n")
	text = newsEmbeds.ReplaceAllString(text, "")
	text = newsItem.ReplaceAllString(text, "* ")
	text = newsHead.ReplaceAllStringFunc(text, func(string) string { return "\n" })
	text = newsTag.ReplaceAllString(text, "")
	text = newsBlank.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}
