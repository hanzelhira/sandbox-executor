// Package web embeds the built frontend so the server ships as one binary.
//
// go:embed can only reach files at or below its own package directory, which
// is why this lives in web/ rather than cmd/server.
//
// dist/ is build output and is gitignored apart from a placeholder, so a
// fresh clone still compiles — it just serves the "frontend not built"
// message from StaticHandler until `npm run build --prefix web` has run.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Assets returns the built frontend rooted at dist/.
func Assets() (fs.FS, error) { return fs.Sub(assets, "dist") }
