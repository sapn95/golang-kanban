// What answers about the process rather than about a board: the probes, the
// build, and the three files that make the board installable.
// Package web serves the HTMX front-end. Handlers parse the request, call
// one service method and render a template; there is no business logic and
// no SQL here.
package web

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
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

// --- middleware ---------------------------------------------------------------

// staticHandler serves embedded files with a long cache and no directory
// listings.
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
// with no-cache: these three are read once per load and a stale one is a board
// that will not install.
func (s *Server) rootAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	b, err := fs.ReadFile(assets.FS(), name)
	if err != nil {
		s.log.Error("root asset", "name", name, "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

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
