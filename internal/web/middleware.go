// The wrappers every request passes through, outermost first: logging,
// recover, security headers, the cross-site check and the body cap.

package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// staticHandler serves the embedded asset tree. No directory listing, and a
// long max-age that is honest because every URL carries the tree's digest.
func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// immutable is honest now: the URL carries the digest of the tree, so
		// a file at a given URL never changes and the browser never has to ask.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

// WriteHeader records the status so the log line can print it, then passes it on.
func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write fills in the 200 that a handler writing a body without a status implies.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// crossSite refuses a state-changing request that a page on another origin
// made the browser send.
//
// The board has no CSRF token because it has no session of its own: the cookie
// that authenticates a request belongs to whatever sits in front, and a form on
// an attacker's page rides it. Sec-Fetch-Site is the check that does not need
// state — the browser sets it and a page cannot forge it — and Origin is the
// fallback for anything that does not send it.
func (s *Server) crossSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
			// none is a user-initiated navigation: typing the URL, a
			// bookmark. There is no other page involved.
		case "same-site", "":
			// same-site is a DIFFERENT origin on the same registrable domain.
			// A board at kanban.example.com and anything at any other name
			// under example.com are same-site, so accepting it on its own
			// meant a page on a sibling host could post here with the reader's
			// cookies attached. This board is published beside its siblings on
			// purpose, which is what turns that from a hypothetical into the
			// arrangement it actually runs in.
			//
			// The empty case is a browser that sends no Fetch Metadata at all.
			// Both fall through to Origin, which every browser sends on a
			// cross-origin write.
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					s.log.Warn("refused a cross-site write", "origin", o, "path", r.URL.Path)
					deny(w, r, http.StatusForbidden, "cross-site request")
					return
				}
			}
		default:
			s.log.Warn("refused a cross-site write",
				"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "path", r.URL.Path)
			deny(w, r, http.StatusForbidden, "cross-site request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxBody is what any single request may send. The largest legitimate one is a
// card with a long description, which is capped at 20000 characters by the
// service.
const maxBody = 1 << 20

// limitBody caps the request body before any handler reads it. Without this,
// FormValue falls through to ParseMultipartForm, which spills anything over
// 32MB to a temporary file on the node.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// secureHeaders sets the response headers that do not depend on the request.
//
// The CSP allows inline script and style because the page needs both today:
// layout.html sets the theme in a <script> before the first paint and ten
// handlers are inline onclick/onsubmit attributes, and a label's colour is a
// style attribute, which no nonce covers. Compiling Tailwind (docs/adr/0011)
// took away the third reason, its runtime <style> injection, and left these
// two. Everything else is locked to this origin.
//
// Violations are reported back to this process. A policy this narrow is worth
// having and it is also the kind of thing that breaks one person's browser and
// nobody else's: something injects a script, or a proxy rewrites a page, and
// what they see is a feature that does not work rather than a rule that refused
// it. Both ways of asking are used, because they are at different points in
// their lives: report-uri is deprecated and is what Firefox and Safari send,
// report-to with Reporting-Endpoints is where Chrome is going.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
		"report-uri " + cspReportPath + "; report-to csp"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Reporting-Endpoints", `csp="`+cspReportPath+`"`)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// The page carries the viewer's address, so it must not be kept by a
		// shared cache. Assets set their own Cache-Control and are untouched.
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// logging writes one line per request: method, path, status and how long it took.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "duration", time.Since(start).Round(time.Microsecond).String(), "remote", r.RemoteAddr)
	})
}

// recover turns a panic into a 500 and a log line rather than a dropped
// connection, and keeps the process up for everybody else.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "err", fmt.Sprint(rec))
				deny(w, r, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
