package api

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// unbuilt is what the panel serves when the binary was compiled without the SPA — a bare
// `go build` rather than `make build`. It is a page and not a 500, because the API is fine
// and the operator's next move is a build command, not a bug report.
const unbuilt = `<!doctype html><meta charset="utf-8"><title>Valmin</title>` +
	`<p>The web interface was not built into this binary. Run <code>make build</code>.</p>`

// SPA serves the embedded single-page app: assets by path, and index.html for anything
// else, so a hard refresh on a client-side route works (`06 §4`, adapter-static's fallback).
//
// It is registered on "/" and never sees an /api path. http.ServeMux takes the most
// specific pattern, so the "/api/" route wins — which is what makes `11 §8.2` structural
// rather than a rule someone has to remember. An unmatched API path is a JSON 404 from the
// router's own dispatch; if this handler could answer it, the SPA fallback would hand
// `fetch()` a 200 with a body of HTML, and the parse error names neither the URL nor the
// real problem.
func SPA(assets fs.FS) http.Handler {
	// build/app, because the adapter empties its own output directory and the placeholder
	// that keeps build/ in a fresh checkout has to survive that (ADR-092).
	build, err := fs.Sub(assets, "build/app")
	if err != nil {
		build = assets
	}
	index, indexErr := fs.ReadFile(build, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")

		if indexErr != nil {
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, unbuilt)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			serveIndex(w, index)
			return
		}

		file, err := build.Open(name)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "", http.StatusInternalServerError)
				return
			}
			// A client-side route, not a missing asset. index.html is the answer, and the
			// SPA's own router decides whether the path means anything.
			serveIndex(w, index)
			return
		}
		defer func() { _ = file.Close() }()

		info, err := file.Stat()
		if err != nil || info.IsDir() {
			serveIndex(w, index)
			return
		}

		// Everything under _app/immutable carries a content hash in its name, so it can
		// be cached forever; index.html must not be, or a deploy leaves browsers running the
		// previous app against the new API.
		if strings.HasPrefix(name, "_app/immutable/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		if seeker, ok := file.(io.ReadSeeker); ok {
			http.ServeContent(w, r, info.Name(), info.ModTime(), seeker)
			return
		}
		http.ServeFileFS(w, r, build, name)
	})
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}
