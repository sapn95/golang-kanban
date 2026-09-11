// Package assets embeds the static files served under /assets/: vendored
// third-party JS/CSS (see VERSIONS) and the app's own script and stylesheet.
// Nothing is loaded from a CDN at runtime.
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"sync"
)

// tailwind.css is compiled from the classes the templates and app.js use; it is
// committed, so building the binary needs nothing but Go.
//
//go:generate sh tailwind.sh

// Only what is served: tailwind.sh, tailwind.config.js and tailwind.input.css
// are the compile's inputs and have no business in the binary.
//
// manifest.webmanifest and sw.js are embedded here and served from the root, not
// from /assets/: a service worker controls only what is under the path it came
// from, and a manifest's scope defaults to its own directory. The PNGs are the
// touch icon Safari wants, which has never taken an SVG, and the two sizes an
// Android install reads out of the manifest.
//
//go:embed vendor app.js app.css tailwind.css logo.svg manifest.webmanifest sw.js icon-180.png icon-192.png icon-512.png
var files embed.FS

// FS is the embedded file tree, rooted at the assets directory.
func FS() fs.FS { return files }

var version = sync.OnceValue(computeVersion)

// Version is a short digest of everything in this package. It goes in the
// query string of every asset URL.
//
// The files are served with a long max-age and no validator — an embedded file
// has no modification time, so the browser has nothing to revalidate against
// and simply keeps what it has until the max-age runs out, which is a year.
// That is correct for a file
// that never changes and wrong for one that changes on every deploy: it left
// browsers running the previous build's JavaScript against the new build's
// HTML, with no error anywhere to say so.
//
// A digest rather than the build version, so it changes when the assets change
// and not when an unrelated line of Go does, and so a binary built without
// ldflags gets a correct one too.
func Version() string { return version() }

func computeVersion() string {
	sum := sha256.New()
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// The name is hashed as well as the content, so renaming a file
		// changes the digest even when nothing inside it did.
		if _, err := io.WriteString(sum, path+"\x00"); err != nil {
			return err
		}
		f, err := files.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(sum, f)
		return err
	})
	if err != nil {
		// Only reachable if the embedded tree is unreadable, which cannot
		// happen in a built binary. An empty version means no cache busting
		// rather than a panic on a working server.
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}
