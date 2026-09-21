package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// modSummary is one mod_packages row on the wire. Description/LatestVersion/Downloads/
// IconURL are fields the sync derives — it already resolved them, so this layer
// only decodes CategoriesJSON back into a real array.
type modSummary struct {
	FullName string `json:"full_name"`
	// Source is the registry this listing came from. A package both registries carry appears
	// as one row per registry, each with its own versions (B14).
	Source        string   `json:"source"`
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

// toModSummary never fails the request over a malformed categories value: one bad row degrades
// to an empty list with a logged warning rather than a 500 for every caller whose page crosses
// it. Categories are decorative here, the install path reading the store row directly.
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
		FullName: p.FullName, Source: p.Source.String(),
		Namespace: p.Namespace, Name: p.Name, Description: p.Description,
		LatestVersion: p.LatestVersion, Downloads: p.Downloads, Rating: p.Rating,
		IsDeprecated: p.IsDeprecated, Categories: categories, IconURL: p.IconURL,
	}
}

// modSearchResponse is GET /mods/search's body. SyncedAt is page-level rather than per-row,
// since one sync updates every package at once; it reads the kv key syncRun writes, and is null
// before the first sync.
type modSearchResponse struct {
	Items      []modSummary     `json:"items"`
	NextCursor *string          `json:"next_cursor"`
	SyncedAt   *string          `json:"synced_at"`
	Registries []registryStatus `json:"registries"`
}

// mayBrowse gates both handlers in this file. It is not a single Can() call because there is no
// per-catalogue-row action to check against an instance: mods.list sits on every grant role
// (09 §3.1), so holding a live grant anywhere is the same question authz.VisibleInstances
// answers.
//
// The denial is 403 rather than 404: D2's existence oracle is about a caller-supplied instance
// id, and the catalogue carries no such identity.
//
// Writes the response and reports false when the caller should be turned away.
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
	src, ok := requestedSource(w, r)
	if !ok {
		return
	}

	// One more than asked for: the extra row is how the page knows there is a next one,
	// the same trick jobHistory (telemetry.go) uses (11 §4).
	rows, err := m.DB.SearchModPackages(r.Context(), &store.ModSearch{
		Query: q, Category: category, Source: src, Sources: m.enabledSources(),
		AfterSortKey: cursor.SortKey, AfterRowKey: cursor.ID, Limit: limit + 1,
	})
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: last.SearchSortKey, ID: last.SearchRowKey}.Encode()
		next = &encoded
	}

	items := make([]modSummary, 0, len(rows))
	for i := range rows {
		items = append(items, toModSummary(r.Context(), &rows[i]))
	}

	statuses := m.registryStatuses(r)
	JSON(w, r, http.StatusOK, modSearchResponse{
		Items: items, NextCursor: next, SyncedAt: catalogueSyncedAt(statuses, src), Registries: statuses,
	})
}

// modVersionView is one mod_versions row on the wire.
type modVersionView struct {
	Version      string   `json:"version"`
	Source       string   `json:"source"`
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

// packageDetail is GET /mods/{namespace}/{name} (04 §3): the cached package plus its version
// history. See mayBrowse for the authorization.
//
// fullName concatenates the two path segments as "namespace-name" (03 §6.2), which assumes a
// namespace contains no hyphen. That is unconfirmed but matches the upstream routing this
// mirrors, where package_url separates them with "/". full_name is the primary key, so a wrong
// split resolves to "not found" rather than to the wrong package.
func (m *Mods) packageDetail(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !m.mayBrowse(w, r, u) {
		return
	}

	fullName := r.PathValue("namespace") + "-" + r.PathValue("name")

	src, ok := requestedSource(w, r)
	if !ok {
		return
	}

	// Keep disabled registries' cached metadata readable for installed mods; resolution
	// enforces whether their packages may be downloaded.
	// Only the enabled registries: a disabled one's rows stay in the index, and offering
	// them here would advertise versions and download URLs the install path refuses.
	pkg, err := m.indexedPackage(r.Context(), fullName, src, m.enabledSources())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	// A named registry that does not carry the package is a miss, not a fallback: the
	// operator asked about that registry's listing.
	if pkg == nil || (src != (source.Source{}) && pkg.Source != src) {
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
		if v.Source != pkg.Source {
			continue
		}
		views = append(views, toModVersionView(r.Context(), &v))
	}

	JSON(w, r, http.StatusOK, modDetailResponse{modSummary: toModSummary(r.Context(), pkg), Versions: views})
}

// toModVersionView applies toModSummary's leniency to one mod_versions row: a malformed
// dependencies value empties that version's list rather than failing the detail page. The
// resolver reads DependenciesJSON off the store row, not through this display struct.
func toModVersionView(ctx context.Context, v *store.ModVersion) modVersionView {
	var deps []string
	if v.DependenciesJSON != "" {
		if err := json.Unmarshal([]byte(v.DependenciesJSON), &deps); err != nil {
			slog.WarnContext(ctx, "malformed mod_versions.dependencies, showing none",
				slog.String("full_name", v.FullName), slog.String("version", v.Version), slog.Any("error", err))
			deps = nil
		}
	}
	return modVersionView{
		Version: v.Version, Source: v.Source.String(), Dependencies: deps,
		DownloadURL: v.DownloadURL, FileSize: v.FileSize,
	}
}

type registryStatus struct {
	Source   string  `json:"source"`
	Enabled  bool    `json:"enabled"`
	SyncedAt *string `json:"synced_at"`
}

func (m *Mods) registryStatuses(r *http.Request) []registryStatus {
	statuses := make([]registryStatus, 0, len(source.All()))
	for _, src := range source.All() {
		_, enabled := m.Clients[src]
		status := registryStatus{Source: src.String(), Enabled: enabled}
		var stamp string
		if ok, err := m.DB.KVGet(r.Context(), kvSyncedAt(src), &stamp); err == nil && ok && stamp != "" {
			status.SyncedAt = &stamp
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// catalogueSyncedAt reports freshness only for the registries in the selected catalogue.
func catalogueSyncedAt(statuses []registryStatus, selected source.Source) *string {
	var oldest *string
	for _, status := range statuses {
		if !status.Enabled || (selected != (source.Source{}) && status.Source != selected.String()) {
			continue
		}
		if status.SyncedAt == nil {
			return nil
		}
		if oldest == nil || *status.SyncedAt < *oldest {
			oldest = status.SyncedAt
		}
	}
	return oldest
}

// requestedSource reads the optional `source` query parameter. Its absence means every
// registry; a name no build recognises is a bad request rather than an empty result, since
// the alternative is a filter that silently matched nothing.
func requestedSource(w http.ResponseWriter, r *http.Request) (source.Source, bool) {
	name := strings.TrimSpace(r.URL.Query().Get("source"))
	if name == "" {
		return source.Source{}, true
	}
	src, ok := source.ByName(name)
	if !ok {
		apierr.Write(w, r, apierr.New(apierr.InvalidParameter).
			Msg("source must name a configured mod registry").With("source", name))
		return source.Source{}, false
	}
	return src, true
}
