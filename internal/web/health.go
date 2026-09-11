// What answers about the process rather than about a board: the probes, the
// build, and the three files that make the board installable.

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"kanban/assets"
	"kanban/internal/identity"
)

// buildInfo says which build is answering.
//
// Without it, telling whether a deploy actually landed means fetching a page
// and looking for markup that only the new version renders, which is guesswork
// dressed up as a check. The release workflow passes the commit in, so this is
// the same string that is in the git history.
//
// It is behind whatever guards the hostname, like every other route except the
// two probes. On a LAN address it is readable, which is the point: knowing the
// version is how you find out that a rollout is stuck.
func (s *Server) buildInfo(w http.ResponseWriter, r *http.Request) {
	version, commit := s.version, s.commit
	if version == "" {
		version = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	plain(w, http.StatusOK, fmt.Sprintf("kanban %s (commit %s)\n", version, commit))
}

// readyz says whether the database answers. Distinct from healthz on purpose: a
// process that is up and cannot reach its store should not be sent traffic.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.ready != nil {
		if err := s.ready(ctx); err != nil {
			s.log.Warn("not ready", "err", err)
			plain(w, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	plain(w, http.StatusOK, "ready")
}

// manifest and serviceWorker serve two files out of the embedded tree at the
// root rather than under /assets/. Neither carries the digest in its URL, so
// neither can be cached for a year the way an asset is: a manifest is read once
// on install and a service worker is checked for a new copy on every load, and
// a stale one of either is a board that will not update.
func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	s.rootAsset(w, r, "manifest.webmanifest", "application/manifest+json")
}

// serviceWorker serves /sw.js from the root, because a worker only controls
// what is under the path it came from.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	s.rootAsset(w, r, "sw.js", "text/javascript; charset=utf-8")
}

// rootAsset serves one embedded file from the root rather than from /assets/,
// with no-cache: both are read on a load and a stale one of either is a board
// that will not install.
func (s *Server) rootAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	b, err := fs.ReadFile(assets.FS(), name)
	if err != nil {
		s.log.Error("root asset", "name", name, "err", err)
		http.NotFound(w, r)
		return
	}
	// The worker names its cache after the build, and it is the one file here
	// that has to differ between releases: a browser reinstalls a worker only
	// when the worker's own bytes change, and without this they never did, so
	// the offline page a browser had cached was the first one it ever saw.
	body := bytes.ReplaceAll(b, []byte(assetVersionMarker), []byte(contentVersion()))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// assetVersionMarker is what sw.js carries where the build's digest goes.
const assetVersionMarker = "__ASSET_VERSION__"

// contentVersion is a digest of everything the worker's cache could be holding:
// the asset tree and the templates.
//
// The asset digest alone was not enough. The one thing the worker caches is the
// offline page, which is a template, so a release that changed nothing but that
// page left /sw.js byte for byte identical, the browser never reinstalled it,
// and the page it kept serving offline was the old one. A cache key has to
// cover what is in the cache.
var contentVersion = sync.OnceValue(func() string {
	sum := sha256.New()
	_, _ = io.WriteString(sum, assets.Version())
	_ = fs.WalkDir(templateFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templateFiles, path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(sum, path+"\x00")
		_, _ = sum.Write(b)
		return nil
	})
	return hex.EncodeToString(sum.Sum(nil))[:12]
})

// offline is what the service worker answers with when a navigation cannot
// reach the server. It says so in the board's own words rather than leaving the
// browser to draw its error page over an installed application.
func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.pages["offline"], "layout", http.StatusOK,
		offlinePage{Title: "Offline", User: identity.FromContext(r.Context())})
}

type offlinePage struct {
	Title string
	User  identity.User
	// BoardSlug is what layout.html reads for the body attribute; there is no
	// board here, and an empty one leaves the attribute off.
	BoardSlug string
}
