package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/store"
)

// modSummary is one mod_packages row on the wire. Description/LatestVersion/Downloads/
// IconURL are fields the sync derives — it already resolved them, so this layer
// only decodes CategoriesJSON back into a real array.
type modSummary struct {
	FullName      string   `json:"full_name"`
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	LatestVersion string   `json:"latest_version"`
	Downloads     int64    `json:"downloads"`
	Rating        int      `json:"rating"`
	IsDeprecated  bool     `json:"is_deprecated"`
	Categories    []string `json:"categories"`
	IconURL       string   `json:"icon_url"`
}

// toModSummary never fails the request over a malformed categories value. Categories are
// decorative here — the install path reads DependenciesJSON and the rest straight off the
// store row, never through this struct — so one bad row degrades to an empty list with a
// logged warning. Returning an error instead would turn a single malformed row into a 500
// for every caller whose page happens to cross it.
func toModSummary(ctx context.Context, p *store.ModPackage) modSummary {
	var categories []string
	if p.CategoriesJSON != "" {
		if err := json.Unmarshal([]byte(p.CategoriesJSON), &categories); err != nil {
			slog.WarnContext(ctx, "malformed mod_packages.categories, showing none",
				slog.String("full_name", p.FullName), slog.Any("error", err))
			categories = nil
		}
	}
	return modSummary{
		FullName: p.FullName, Namespace: p.Namespace, Name: p.Name, Description: p.Description,
		LatestVersion: p.LatestVersion, Downloads: p.Downloads, Rating: p.Rating,
		IsDeprecated: p.IsDeprecated, Categories: categories, IconURL: p.IconURL,
	}
}

// modSearchResponse is GET /mods/search's body. SyncedAt is page-level, not per-row: one
// sync updates every package at once, so there is exactly one freshness figure for the
// whole response, sourced from the same kv key syncRun writes (mods.go). It is null before
// the first sync has ever run.
type modSearchResponse struct {
	Items      []modSummary `json:"items"`
	NextCursor *string      `json:"next_cursor"`
	SyncedAt   *string      `json:"synced_at"`
}

// mayBrowse gates both handlers in this file.
//
// Not a single Can() call, by the same reasoning instances.go:list and permissions.go:mine
// already use: there is no per-catalogue-row action to check against a specific instance.
// mods.list sits on every grant role (09 §3.1), so "holds a live grant of any role on any
// instance" is exactly "holds mods.list somewhere", which authz.VisibleInstances already
// answers.
//
// The denial is 403, not 404, deliberately rather than by oversight of D2: a 403 on a
// caller-supplied instance id is an existence oracle, and the mod catalogue carries no such
// caller-supplied identity to leak.
//
// It writes the response and reports false when the caller should be turned away.
func (m *Mods) mayBrowse(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	ids, all, err := m.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if !all && len(ids) == 0 {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return false
	}
	return true
}

// search is GET /mods/search (04 §3): `LIKE` over the cached index (Decision 6), never
// the live Thunderstore API. See mayBrowse for the authorization.
func (m *Mods) search(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !m.mayBrowse(w, r, u) {
		return
	}

	limit, err := ParseLimit(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	cursor, _, err := ParseCursor(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	category := strings.TrimSpace(r.URL.Query().Get("category"))

	// One more than asked for: the extra row is how the page knows there is a next one,
	// the same trick jobHistory (telemetry.go) uses (11 §4).
	rows, err := m.DB.SearchModPackages(r.Context(), q, category, cursor.SortKey, cursor.ID, limit+1)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: last.SearchSortKey, ID: last.FullName}.Encode()
		next = &encoded
	}

	items := make([]modSummary, 0, len(rows))
	for i := range rows {
		items = append(items, toModSummary(r.Context(), &rows[i]))
	}

	JSON(w, r, http.StatusOK, modSearchResponse{Items: items, NextCursor: next, SyncedAt: m.syncedAt(r)})
}

// modVersionView is one mod_versions row on the wire.
type modVersionView struct {
	Version      string   `json:"version"`
	Dependencies []string `json:"dependencies"`
	DownloadURL  string   `json:"download_url"`
	FileSize     int64    `json:"file_size"`
}

// modDetailResponse embeds modSummary so its fields promote to the top level, alongside
// the version history 04 §3 asks for.
type modDetailResponse struct {
	modSummary
	Versions []modVersionView `json:"versions"`
}

// packageDetail is GET /mods/{namespace}/{name} (04 §3): the cached package plus its
// version history. See mayBrowse for the authorization.
//
// fullName is built by concatenating the two path segments as "namespace-name"
// (03 §6.2's own notation). This trusts that a Thunderstore namespace never itself
// contains a hyphen — unconfirmed, but Thunderstore's own
// package_url separates them with "/", not "-" (03 §6.1's captured URLs, e.g.
// ".../p/ValheimModding/Jotunn/"), which is the same assumption baked into the upstream
// routing this endpoint mirrors. mod_packages.full_name is the primary key, so a wrong
// split resolves to "not found" rather than the wrong package — never a collision.
func (m *Mods) packageDetail(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !m.mayBrowse(w, r, u) {
		return
	}

	fullName := r.PathValue("namespace") + "-" + r.PathValue("name")

	pkg, err := m.DB.ModPackageByFullName(r.Context(), fullName)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if pkg == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	versions, err := m.DB.ModVersionsByFullName(r.Context(), fullName)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	views := make([]modVersionView, 0, len(versions))
	for _, v := range versions {
		views = append(views, toModVersionView(r.Context(), &v))
	}

	JSON(w, r, http.StatusOK, modDetailResponse{modSummary: toModSummary(r.Context(), pkg), Versions: views})
}

// toModVersionView is toModSummary's leniency for one mod_versions row: a malformed
// dependencies value degrades that version's list to empty rather than 500ing the whole
// detail page. The real dependency graph the resolver walks reads
// DependenciesJSON off the store row directly, not through this display struct.
func toModVersionView(ctx context.Context, v *store.ModVersion) modVersionView {
	var deps []string
	if v.DependenciesJSON != "" {
		if err := json.Unmarshal([]byte(v.DependenciesJSON), &deps); err != nil {
			slog.WarnContext(ctx, "malformed mod_versions.dependencies, showing none",
				slog.String("full_name", v.FullName), slog.String("version", v.Version), slog.Any("error", err))
			deps = nil
		}
	}
	return modVersionView{Version: v.Version, Dependencies: deps, DownloadURL: v.DownloadURL, FileSize: v.FileSize}
}

// syncedAt reads kv's freshness stamp for the search/detail responses. Any failure —
// including "no sync has ever run" — degrades to null rather than failing the request:
// this is informational, not a correctness boundary.
func (m *Mods) syncedAt(r *http.Request) *string {
	var raw string
	if ok, err := m.DB.KVGet(r.Context(), kvThunderstoreSyncedAt, &raw); err != nil || !ok {
		return nil
	}
	return &raw
}
