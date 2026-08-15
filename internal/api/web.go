package api

import (
	"io/fs"
	"net/http"
	"strings"
)

// StaticHandler serves the embedded frontend. The caller passes the built
// assets (see cmd/server, which embeds web/dist), so this package stays
// independent of where they came from.
//
// Unknown paths fall back to index.html: the frontend is a single-page app,
// so a deep link must return the app rather than a 404. API routes are
// registered separately on the mux and are matched first.
func StaticHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if _, err := fs.Stat(assets, path); err != nil {
			// Not a real file: serve the SPA shell.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			serveIndex(w, r, assets)
			return
		}

		// Hashed asset filenames are content-addressed, so they can be cached
		// hard. index.html must not be, or a stale shell would keep pointing
		// at assets from a previous build.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "frontend not built: run `npm run build --prefix web`", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
