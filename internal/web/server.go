// Package web serves the HTMX front-end. Handlers parse the request, call
// one service method and render a template; there is no business logic and
// no SQL here.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"kanban/assets"
	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

// Server is the HTTP front-end.
type Server struct {
	svc     *service.Kanban
	ready   func(context.Context) error
	log     *slog.Logger
	now     func() time.Time
	version string
	commit  string
	avatars *avatars                      // nil unless pictures are configured
	viewers []identity.User               // empty unless a roster is configured
	api     http.Handler                  // nil unless the JSON API is mounted
	pages   map[string]*template.Template // full pages, keyed by name
	parts   *template.Template            // fragments: card, card_edit
}

// apiPrefix is where WithAPI mounts its handler. It is api.Prefix written out:
// the package that serves pages does not import the one that serves JSON, and
// server_test.go holds the two spellings together.
const apiPrefix = "/api/v1/"

// Option configures New.
type Option func(*Server)

// WithClock replaces time.Now, used for the overdue marker.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithBuild records what is running, for /version. Leave it out and the
// endpoint says so rather than making something up.
func WithBuild(version, commit string) Option {
	return func(s *Server) { s.version, s.commit = version, commit }
}

// WithAvatars shows people's pictures instead of their initials, for the
// addresses in the map, which maps an address to a GitHub login. Leave it out
// and the board keeps the initials it has always drawn and this process makes
// no outbound request. See avatar.go for why the pictures are proxied.
func WithAvatars(logins map[string]string) Option {
	return func(s *Server) { s.avatars = newAvatars(logins) }
}

// WithViewers tells the board who may see it, so the app bar can say so.
//
// The list is not consulted for anything. What decides who gets in is the
// sign-in in front of the board, and this is a copy of that decision written
// where the people it concerns will read it. Passing a copy that has drifted
// makes the page wrong and changes nothing about who is let in.
func WithViewers(people []identity.User) Option {
	return func(s *Server) { s.viewers = people }
}

// WithAPI mounts the JSON API under apiPrefix. Pass api.New(svc, log); it is
// taken as an http.Handler so that this package does not depend on that one.
//
// Leave it out and the process answers nothing but pages, which is the
// deployment that has a board on the open internet behind a proxy and no
// scripts against it.
func WithAPI(h http.Handler) Option { return func(s *Server) { s.api = h } }

// New builds the handler. ready is called by /readyz; pass the store's Ping.
func New(svc *service.Kanban, ready func(context.Context) error, log *slog.Logger, opts ...Option) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{svc: svc, ready: ready, log: log, now: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(s)
	}
	s.parseTemplates()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	// The list, always. GET / redirects to the board when there is only one,
	// which is what somebody opening the bookmark wants and also meant the page
	// that creates a second board could not be reached while there was one.
	mux.HandleFunc("GET /boards", s.boardList)
	mux.HandleFunc("POST /boards", s.createBoard)
	mux.HandleFunc("GET /b/{board}", s.board)
	mux.HandleFunc("POST /b/{board}/cards", s.createCard)
	mux.HandleFunc("POST /b/{board}/columns/{column}/order", s.reorderCards)
	mux.HandleFunc("GET /subtask-row", s.subtaskRow)
	mux.HandleFunc("POST /b/{board}/cards/bulk", s.bulkCards)
	mux.HandleFunc("GET /cards/{id}", s.card)
	mux.HandleFunc("GET /cards/{id}/edit", s.editCard)
	mux.HandleFunc("POST /cards/{id}", s.updateCard)
	// The quick edits from the card face. Each writes one field, so they are
	// not the update form with most of its inputs left out.
	mux.HandleFunc("POST /cards/{id}/assignee", s.setCardAssignee)
	mux.HandleFunc("POST /cards/{id}/due", s.setCardDue)
	mux.HandleFunc("POST /cards/{id}/labels/{label}/toggle", s.toggleCardLabel)
	mux.HandleFunc("POST /cards/{id}/subtasks/{subtask}/toggle", s.toggleSubtask)
	mux.HandleFunc("POST /cards/{id}/delete", s.deleteCard)
	mux.HandleFunc("POST /cards/{id}/archive", s.archiveCard)
	mux.HandleFunc("POST /cards/{id}/restore", s.restoreCard)
	mux.HandleFunc("POST /cards/{id}/comments", s.addComment)
	mux.HandleFunc("POST /comments/{id}/delete", s.deleteComment)
	mux.HandleFunc("GET /b/{board}/archive", s.archive)
	// One settings page for the two things a board owns besides its cards.
	// The old /labels URL is kept, because it shipped, and a bookmark to it
	// should land somewhere rather than 404.
	mux.HandleFunc("GET /b/{board}/settings", s.settings)
	mux.HandleFunc("GET /b/{board}/labels", s.settingsMoved)
	mux.HandleFunc("POST /b/{board}/labels", s.createLabel)
	mux.HandleFunc("POST /b/{board}/labels/{id}", s.updateLabel)
	mux.HandleFunc("POST /b/{board}/labels/{id}/delete", s.deleteLabel)
	mux.HandleFunc("POST /b/{board}/columns", s.createColumn)
	mux.HandleFunc("POST /b/{board}/columns/{id}", s.updateColumn)
	mux.HandleFunc("POST /b/{board}/columns/{id}/delete", s.deleteColumn)
	mux.HandleFunc("POST /b/{board}/columns/{id}/move", s.moveColumn)
	mux.HandleFunc("POST /b/{board}/delete", s.deleteBoard)
	mux.HandleFunc("POST /b/{board}/layout", s.setLayout)
	mux.HandleFunc("POST /b/{board}/sla", s.setSLA)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", staticHandler(http.FileServerFS(assets.FS()))))
	// The JSON API, on this mux and so inside the same middleware: one body cap,
	// one cross-site check, one log line per request, and identity read once for
	// both kinds of caller. It brings its own routes, which is why this
	// registration names no method.
	if s.api != nil {
		mux.Handle(apiPrefix, s.api)
	}
	// Registered only when there are pictures to serve, so a board without them
	// has no route that reaches out of the process at all.
	if s.avatars != nil {
		mux.HandleFunc("GET /avatar/{login}", s.serveAvatar)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { plain(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", s.buildInfo)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// The three that make the board installable. All at the root, because a
	// service worker only controls what is under the path it was served from
	// and a manifest's scope defaults to its own directory: from /assets/ both
	// would cover the assets and nothing else.
	// Spelled out rather than built from cspReportPath, because the endpoint
	// reference is checked against the literals in this function; a test holds
	// the two spellings together, the way it does for the API prefix.
	mux.HandleFunc("POST /csp-report", s.cspViolation)
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	mux.HandleFunc("GET /offline", s.offline)
	return s.logging(s.recover(s.secureHeaders(s.crossSite(s.limitBody(mux)))))
}

// parseTemplates compiles every page once at startup. The fragments are parsed
// into a base the pages clone, so a card looks the same wherever it is drawn.
func (s *Server) parseTemplates() {
	funcs := template.FuncMap{
		"hasLabel": func(c model.Card, id model.ID) bool {
			for _, l := range c.Labels {
				if l == id {
					return true
				}
			}
			return false
		},
		// An assignee is an address, and a card is narrow. These two keep
		// the display logic out of the template, where a wrong byte offset
		// would silently cut a multi-byte character in half.
		"initials":   func(addr string) string { return identity.User{Email: addr}.Initials() },
		"personName": func(addr string) string { return identity.User{Email: addr}.Display() },
		// "" for anybody without a configured picture, which is everybody
		// unless WithAvatars was passed. The bubble draws initials on "".
		"avatar":     s.avatarURL,
		"readableOn": readableOn,
		// A card description is markdown. The renderer escapes the source
		// before it looks at it, which is what allows its result to be marked
		// as HTML here; markdown.go carries that argument in full.
		"markdown": renderMarkdown,
		// Every asset URL carries the digest of the embedded tree, so a new
		// build is a new URL and the browser cannot serve yesterday's script
		// against today's markup.
		"asset": func(path string) string {
			if v := assets.Version(); v != "" {
				return "/assets/" + path + "?v=" + v
			}
			return "/assets/" + path
		},
		// The footer prints what is running. On a deployment that rolls out by
		// tag, the question "is this the build I merged" is asked at the board
		// and answered by reading the page rather than by curling /version.
		"build": func() string {
			if s.version == "" {
				return "dev"
			}
			return s.version
		},
		// Whether the JSON API is mounted, so the footer links to it where it
		// exists and says nothing where it does not.
		"hasAPI": func() bool { return s.api != nil },
		// How much JavaScript this project wrote, counted out of the embedded
		// trees on first use. The footer prints it beside the version, because
		// a board whose whole idea is a server-rendered page is one where that
		// number going up should be visible to whoever let it.
		"js": countJS,
		// Who may see this board. A function rather than a field on all five
		// page structs, because it is the same answer on every page and none
		// of the handlers has anything to add to it.
		"viewers": func() []identity.User { return s.viewers },
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFiles,
		"templates/layout.html", "templates/card.html", "templates/card_edit.html", "templates/comment.html",
		"templates/column.html"))
	s.parts = base
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"board", "boards", "archive", "settings", "offline"} {
		s.pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
}

// plain writes a bare text response, for the handlers that answer a word.
func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

// deny answers in the shape the caller of that path is owed. The middleware is
// the one place a request under the API prefix is refused without the api
// package ever seeing it, and openapi.json promises an object with an "error"
// field for every failure, including these two.
func deny(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if !strings.HasPrefix(r.URL.Path, apiPrefix) {
		plain(w, status, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// fragment is one template and the data to run it with.
type fragment struct {
	name string
	data any
}

// render executes one template into the response. A template that fails halfway
// has already written part of a page, so it is built in memory first.
func (s *Server) render(w http.ResponseWriter, t *template.Template, name string, status int, data any) {
	s.renderAll(w, t, status, fragment{name, data})
}

// renderAll writes several fragments into one response, in order. htmx applies
// the first to the target and takes any element marked hx-swap-oob out of the
// body and applies it wherever it belongs on the page.
func (s *Server) renderAll(w http.ResponseWriter, t *template.Template, status int, frags ...fragment) {
	var buf strings.Builder
	for _, f := range frags {
		if err := t.ExecuteTemplate(&buf, f.name, f.data); err != nil {
			s.log.Error("render", "template", f.name, "err", err)
			plain(w, http.StatusInternalServerError, "template error")
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.String())
}

// fail maps service and store errors to HTTP status codes.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		plain(w, http.StatusBadRequest, ve.Error())
	case errors.Is(err, store.ErrNotFound):
		plain(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		plain(w, http.StatusConflict, "already exists")
	case errors.Is(err, service.ErrWIPLimit):
		plain(w, http.StatusConflict, service.ErrWIPLimit.Error())
	case errors.Is(err, service.ErrNotAuthor):
		plain(w, http.StatusForbidden, service.ErrNotAuthor.Error())
	case errors.Is(err, store.ErrInvalid):
		plain(w, http.StatusBadRequest, "invalid request")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		plain(w, http.StatusInternalServerError, "internal error")
	}
}

// isHTMX reports whether the request came from htmx, which is the difference
// between answering with a fragment and redirecting to a whole page.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") != "" }

// --- handlers -----------------------------------------------------------------
