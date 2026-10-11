package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/templaterepo"
)

const (
	// templateRefresh is how long a fetched template repository is served before it is
	// fetched again. ?refresh=true skips the wait.
	templateRefresh      = time.Hour
	templateFetchTimeout = 20 * time.Second
)

// Templates serves the template catalogue: every configured template repository, fetched on
// demand and kept in memory.
type Templates struct {
	Authz   *authz.Authz
	Sources []string
	Client  *http.Client

	mu    sync.Mutex
	cache map[string]*templateSource
}

// templateSource is the last good fetch of one repository and the outcome of the last attempt.
type templateSource struct {
	checkedAt time.Time
	fetchedAt time.Time
	etag      string
	result    templaterepo.Result
	err       string
}

func templateRoutes(rt *routeTable, h *Templates) {
	rt.Handle("GET /api/v1/templates", http.HandlerFunc(h.list))
}

type templateCatalogueView struct {
	Sources   []templateSourceView  `json:"sources"`
	Templates []templateView        `json:"templates"`
	Problems  []templateProblemView `json:"problems"`
}

type templateSourceView struct {
	URL       string     `json:"url"`
	FetchedAt *time.Time `json:"fetched_at"`
	Error     *string    `json:"error"`
}

type templateView struct {
	Source      string `json:"source"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Mods        int    `json:"mods"`
	Code        string `json:"code"`
}

type templateProblemView struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// list is GET /templates. Gated on instance.create, since its only use is creating a server.
func (h *Templates) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	force := r.URL.Query().Get("refresh") == "true"

	h.mu.Lock()
	defer h.mu.Unlock()
	view := templateCatalogueView{
		Sources: []templateSourceView{}, Templates: []templateView{}, Problems: []templateProblemView{},
	}
	for _, url := range h.Sources {
		src := h.source(r.Context(), url, force)
		sv := templateSourceView{URL: url}
		if !src.fetchedAt.IsZero() {
			at := src.fetchedAt
			sv.FetchedAt = &at
		}
		if src.err != "" {
			msg := src.err
			sv.Error = &msg
		}
		view.Sources = append(view.Sources, sv)
		for _, t := range src.result.Templates {
			view.Templates = append(view.Templates, templateView{
				Source: url, Slug: t.Slug, Name: t.Name, Description: t.Description, Mods: t.Mods, Code: t.Code,
			})
		}
		for _, p := range src.result.Problems {
			view.Problems = append(view.Problems, templateProblemView{Source: url, Path: p.Path, Detail: p.Detail})
		}
	}
	JSON(w, r, http.StatusOK, view)
}

// source returns url's cache entry, fetching it first when it is stale or force is set. A failed
// fetch keeps the last good templates. The fetch outlives a cancelled request, so leaving the
// page never records a failure. The caller holds h.mu.
func (h *Templates) source(ctx context.Context, url string, force bool) *templateSource {
	if h.cache == nil {
		h.cache = map[string]*templateSource{}
	}
	src := h.cache[url]
	if src == nil {
		src = &templateSource{}
		h.cache[url] = src
	}
	now := time.Now().UTC()
	if !force && now.Sub(src.checkedAt) < templateRefresh {
		return src
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), templateFetchTimeout)
	defer cancel()
	res, err := templaterepo.Fetch(ctx, h.Client, url, src.etag)
	src.checkedAt = now
	if err != nil {
		src.err = err.Error()
		return src
	}
	src.fetchedAt, src.err = now, ""
	if !res.NotModified {
		src.etag, src.result = res.ETag, res
	}
	return src
}
