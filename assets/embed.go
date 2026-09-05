// Package assets embeds the static files served under /assets/: vendored
// third-party JS/CSS (see VERSIONS) and the app's own script and stylesheet.
// Nothing is loaded from a CDN at runtime.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed vendor app.js app.css
var files embed.FS

// FS is the embedded file tree, rooted at the assets directory.
func FS() fs.FS { return files }
