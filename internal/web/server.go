// Package web serves the HTMX front-end. Handlers parse the request, call
// one service method and render a template; there is no business logic and
// no SQL here.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	svc   *service.Kanban
	ready func(context.Context) error
	log   *slog.Logger
	now   func() time.Time
	pages map[string]*template.Template // full pages, keyed by name
	parts *template.Template            // fragments: card, card_edit
}

// Option configures New.
type Option func(*Server)

// WithClock replaces time.Now, used for the overdue marker.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

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
	mux.HandleFunc("POST /boards", s.createBoard)
	mux.HandleFunc("GET /b/{board}", s.board)
	mux.HandleFunc("POST /b/{board}/cards", s.createCard)
	mux.HandleFunc("POST /b/{board}/columns/{column}/order", s.reorderCards)
	mux.HandleFunc("POST /b/{board}/cards/bulk", s.bulkCards)
	mux.HandleFunc("GET /cards/{id}", s.card)
	mux.HandleFunc("GET /cards/{id}/edit", s.editCard)
	mux.HandleFunc("POST /cards/{id}", s.updateCard)
	mux.HandleFunc("POST /cards/{id}/delete", s.deleteCard)
	mux.HandleFunc("POST /cards/{id}/archive", s.archiveCard)
	mux.HandleFunc("POST /cards/{id}/restore", s.restoreCard)
	mux.HandleFunc("GET /b/{board}/archive", s.archive)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", staticHandler(http.FileServerFS(assets.FS()))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { plain(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return s.logging(s.recover(s.secureHeaders(s.crossSite(s.limitBody(mux)))))
}

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
		"initial":      func(addr string) string { return identity.User{Email: addr}.Initial() },
		"shortAddress": func(addr string) string { return identity.User{Email: addr}.Display() },
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/layout.html", "templates/card.html", "templates/card_edit.html"))
	s.parts = base
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"board", "boards", "archive"} {
		s.pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
}

// --- view models --------------------------------------------------------------

type cardView struct {
	// Viewer is who is looking, so a card can offer "assign to me" and mark
	// the ones that are already theirs.
	Viewer      identity.User
	Card        model.Card
	Labels      []model.Label  // resolved from the board
	BoardLabels []model.Label  // every label of the board, for the edit form
	Columns     []model.Column // every column, so the edit form can move the card
	Due         string         // "" or "2 Jan 2006"
	DueInput    string         // "" or "2006-01-02"
	Overdue     bool
}

type columnView struct {
	Column    model.Column
	Cards     []cardView
	Count     int
	OverLimit bool
}

type boardPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Board     *model.Board
	Columns   []columnView
	EmptyCard cardView
}

type archivePage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Board     *model.Board
	Cards     []cardView
}

type boardsPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Boards    []model.Board
	Error     string
}

func (s *Server) cardView(u identity.User, b *model.Board, c model.Card) cardView {
	v := cardView{Viewer: u, Card: c, BoardLabels: b.Labels, Columns: b.Columns}
	for _, id := range c.Labels {
		if l := b.Label(id); l != nil {
			v.Labels = append(v.Labels, *l)
		}
	}
	if !c.DueDate.IsZero() {
		v.Due = c.DueDate.Format("2 Jan 2006")
		v.DueInput = c.DueDate.Format("2006-01-02")
		today := s.now().UTC().Truncate(24 * time.Hour)
		v.Overdue = c.DueDate.Before(today)
	}
	return v
}

func (s *Server) boardPage(u identity.User, b *model.Board, cards []model.Card) boardPage {
	p := boardPage{Title: b.Name, User: u, BoardSlug: b.Slug, Board: b, EmptyCard: cardView{Viewer: u, BoardLabels: b.Labels, Columns: b.Columns}}
	byColumn := map[model.ID][]cardView{}
	for _, c := range cards {
		byColumn[c.ColumnID] = append(byColumn[c.ColumnID], s.cardView(u, b, c))
	}
	for _, col := range b.Columns {
		cv := columnView{Column: col, Cards: byColumn[col.ID], Count: len(byColumn[col.ID])}
		cv.OverLimit = col.WIPLimit > 0 && cv.Count > col.WIPLimit
		p.Columns = append(p.Columns, cv)
	}
	return p
}

// --- rendering and errors -----------------------------------------------------

func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, name string, status int, data any) {
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
		plain(w, http.StatusInternalServerError, "template error")
		return
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
	case errors.Is(err, store.ErrInvalid):
		plain(w, http.StatusBadRequest, "invalid request")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		plain(w, http.StatusInternalServerError, "internal error")
	}
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") != "" }

// --- handlers -----------------------------------------------------------------

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	boards, err := s.svc.Boards(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(boards) == 1 {
		http.Redirect(w, r, "/b/"+boards[0].Slug, http.StatusSeeOther)
		return
	}
	s.render(w, s.pages["boards"], "layout", http.StatusOK, boardsPage{Title: "Kanban", User: identity.FromContext(r.Context()), Boards: boards})
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.CreateBoard(r.Context(), r.FormValue("name"), "", nil)
	if err != nil {
		var ve *service.ValidationError
		if errors.As(err, &ve) || errors.Is(err, store.ErrConflict) {
			boards, lerr := s.svc.Boards(r.Context())
			if lerr != nil {
				s.fail(w, r, lerr)
				return
			}
			msg := "a board with that name already exists"
			if ve != nil {
				msg = ve.Error()
			}
			s.render(w, s.pages["boards"], "layout", http.StatusBadRequest, boardsPage{Title: "Kanban", User: identity.FromContext(r.Context()), Boards: boards, Error: msg})
			return
		}
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cards, err := s.svc.Cards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.pages["board"], "layout", http.StatusOK,
		s.boardPage(identity.FromContext(r.Context()), b, cards))
}

func cardInput(r *http.Request) service.CardInput {
	labels := make([]model.ID, 0, len(r.Form["labels"]))
	for _, l := range r.Form["labels"] {
		labels = append(labels, model.ID(l))
	}
	return service.CardInput{
		Title:       r.FormValue("title"),
		Description: r.FormValue("description"),
		DueDate:     r.FormValue("due_date"),
		Assignee:    r.FormValue("assignee"),
		Labels:      labels,
		Subtasks:    model.ParseSubtasks(r.FormValue("subtasks")),
	}
}

func (s *Server) createCard(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	col := model.ID(r.FormValue("column"))
	if col == "" && len(b.Columns) > 0 {
		col = b.Columns[0].ID
	}
	c, err := s.svc.CreateCard(r.Context(), b.ID, col, cardInput(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
		return
	}
	w.Header().Set("HX-Retarget", "#cards-"+string(c.ColumnID))
	w.Header().Set("HX-Reswap", "beforeend")
	s.render(w, s.parts, "card", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c))
}

type orderPayload struct {
	Order []model.ID `json:"order"`
}

func (s *Server) reorderCards(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var p orderPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		plain(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.svc.ReorderCards(r.Context(), b.ID, model.ID(r.PathValue("column")), p.Order); err != nil {
		s.fail(w, r, err)
		return
	}
	plain(w, http.StatusOK, "OK")
}

// cardAndBoard loads a card and its board for rendering.
// bulkCards applies one action to a set of selected cards.
//
// It answers with HX-Refresh rather than a fragment. A bulk move can empty one
// column and reorder another, and a bulk delete changes counts and WIP
// warnings across the board; stitching that together from partial swaps would
// be a lot of machinery for an action nobody runs in a loop.
func (s *Server) bulkCards(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids := make([]model.ID, 0, len(r.Form["ids"]))
	for _, id := range r.Form["ids"] {
		ids = append(ids, model.ID(id))
	}

	target := r.FormValue("target")
	action := service.BulkAction(r.FormValue("action"))
	if action == service.BulkAssign && target == "@me" {
		// The browser does not know the viewer's address, and asking it to
		// would mean putting the address in the page for scripts to read.
		u := identity.FromContext(r.Context())
		if u.Anonymous() {
			plain(w, http.StatusForbidden, "not signed in")
			return
		}
		target = u.Email
	}

	res, err := s.svc.Bulk(r.Context(), b.ID, action, ids, target)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(res.Failed) > 0 {
		s.log.Warn("bulk action partially failed", "action", action,
			"changed", len(res.Changed), "failed", len(res.Failed))
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cardAndBoard(r *http.Request) (*model.Card, *model.Board, error) {
	c, err := s.svc.Card(r.Context(), model.ID(r.PathValue("id")))
	if err != nil {
		return nil, nil, err
	}
	b, err := s.svc.BoardByID(r.Context(), c.BoardID)
	if err != nil {
		return nil, nil, err
	}
	return c, b, nil
}

func (s *Server) card(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.parts, "card", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c))
}

func (s *Server) editCard(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.parts, "card_edit", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c))
}

func (s *Server) updateCard(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	id := model.ID(r.PathValue("id"))

	// The column moves before the fields are written. The store's UpdateCard
	// deliberately never touches ColumnID, so a move is a reorder, and a
	// reorder can be refused by a WIP limit. Doing it first means a refused
	// move leaves the card exactly as it was, rather than saving the new
	// title into a card that did not go anywhere.
	moved := false
	if want := model.ID(r.FormValue("column")); want != "" {
		cur, err := s.svc.Card(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if cur.ColumnID != want {
			if err := s.svc.ReorderCards(r.Context(), cur.BoardID, want, []model.ID{id}); err != nil {
				s.fail(w, r, err)
				return
			}
			moved = true
		}
	}

	c, err := s.svc.UpdateCard(r.Context(), id, cardInput(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := s.svc.BoardByID(r.Context(), c.BoardID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
		return
	}
	if moved {
		// The form swaps the card in place, which would leave it drawn in the
		// column it just left. Both columns also need their counts and WIP
		// warnings redrawn, so the page is cheaper to redraw than to patch.
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.render(w, s.parts, "card", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c))
}

func (s *Server) deleteCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteCard(r.Context(), model.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// archiveCard takes a card off the board. The card row is removed from the
// page, exactly as a delete does, because from the board's point of view the
// two look the same; the difference is that this one can be undone.
func (s *Server) archiveCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ArchiveCard(r.Context(), model.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) restoreCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RestoreCard(r.Context(), model.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	// The card reappears in a column this page is not showing, so the archive
	// asks the browser to go back to the board rather than patching itself.
	w.Header().Set("HX-Redirect", "/b/"+r.URL.Query().Get("board"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cards, err := s.svc.ArchivedCards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := identity.FromContext(r.Context())
	page := archivePage{Title: b.Name + " · archive", User: u, BoardSlug: b.Slug, Board: b}
	for _, c := range cards {
		page.Cards = append(page.Cards, s.cardView(u, b, c))
	}
	s.render(w, s.pages["archive"], "layout", http.StatusOK, page)
}

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
func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

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
		case "same-origin", "same-site", "none":
			// none is a user-initiated navigation: typing the URL, a
			// bookmark. There is no other page involved.
		case "":
			// No Fetch Metadata at all. Fall back to Origin, which every
			// browser sends on a cross-origin POST.
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					s.log.Warn("refused a cross-site write", "origin", o, "path", r.URL.Path)
					plain(w, http.StatusForbidden, "cross-site request")
					return
				}
			}
		default:
			s.log.Warn("refused a cross-site write",
				"sec_fetch_site", r.Header.Get("Sec-Fetch-Site"), "path", r.URL.Path)
			plain(w, http.StatusForbidden, "cross-site request")
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
// layout.html configures Tailwind in a <script> tag, six handlers are inline
// onclick/onsubmit attributes, and Tailwind's browser build injects a <style>
// element at runtime. Everything else is locked to this origin.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
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

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "err", fmt.Sprint(rec))
				plain(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
