package api

import (
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// maxChangelogBytes bounds the changelog text one response carries.
const maxChangelogBytes = 256 << 10

// changelogView is the CHANGELOG.md a package version ships. RegistryURL is the package's page
// on its registry, empty where the registry's page address is not known.
type changelogView struct {
	Version     string `json:"version"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated"`
	RegistryURL string `json:"registry_url"`
}

// changelog is GET /instances/{id}/mods/{full_name}/changelog?version=&source=: the CHANGELOG.md
// in a package version's zip, 404 when the version ships none. It reads the zip through the same
// cache an install uses, so what is read here is not downloaded again by the install. Like an
// install, it can fill the cache with a download, so it needs mods.manage. The route has no
// request timeout: a large package on a slow link takes longer than one, and the cache ends a
// download that stalls.
func (m *Mods) changelog(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id, fullName := r.PathValue("id"), r.PathValue("full_name")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if version == "" {
		writeFieldError(w, r, "version", apierr.FieldRequired, "version is required.")
		return
	}
	pkg, err := m.changelogPackage(r, fullName)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if pkg == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	zips, ok := m.install.Caches[pkg.Source]
	if !ok {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	url, size, found, err := m.DB.ModVersionDownload(r.Context(), fullName, version, pkg.Source)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !found {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	path, err := zips.Get(r.Context(), fullName+"-"+version, url, size)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Unavailable).Msg("The registry could not be reached.").Wrap(err))
		return
	}
	text, truncated, found, err := extract.ReadRootText(path, "changelog.md", maxChangelogBytes)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !found {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	JSON(w, r, http.StatusOK, changelogView{
		Version:     version,
		Text:        text,
		Truncated:   truncated,
		RegistryURL: m.registryPageURL(pkg.Source, pkg.Namespace, pkg.Name),
	})
}

// changelogPackage finds the package a changelog is asked for: in the registry the request
// names, else in the first enabled registry that lists it.
func (m *Mods) changelogPackage(r *http.Request, fullName string) (*store.ModPackage, error) {
	prefer := source.Source{}
	if name := strings.TrimSpace(r.URL.Query().Get("source")); name != "" {
		src, ok := source.ByName(name)
		if !ok {
			return nil, nil
		}
		prefer = src
	}
	return m.indexedPackage(r.Context(), fullName, prefer, m.enabledSources())
}

// registryPageURL is the package's page on its registry. Only Thunderstore's address is known.
func (m *Mods) registryPageURL(src source.Source, namespace, name string) string {
	client, ok := m.Clients[src]
	if src != source.Thunderstore || !ok || namespace == "" || name == "" {
		return ""
	}
	return strings.TrimRight(client.BaseURL, "/") + "/c/valheim/p/" + namespace + "/" + name + "/"
}
