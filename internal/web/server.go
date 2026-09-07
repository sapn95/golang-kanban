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
	mux.HandleFunc("GET /cards/{id}", s.card)
	mux.HandleFunc("GET /cards/{id}/edit", s.editCard)
	mux.HandleFunc("POST /cards/{id}", s.updateCard)
	mux.HandleFunc("POST /cards/{id}/delete", s.deleteCard)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", staticHandler(http.FileServerFS(assets.FS()))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { plain(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return s.logging(s.recover(mux))
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
	for _, name := range []string{"board", "boards"} {
		s.pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
}

// --- view models --------------------------------------------------------------

type cardView struct {
	// Viewer is who is looking, so a card can offer "assign to me" and mark
	// the ones that are already theirs.
	Viewer      identity.User
	Card        model.Card
	Labels      []model.Label // resolved from the board
	BoardLabels []model.Label // every label of the board, for the edit form
	Due         string        // "" or "2 Jan 2006"
	DueInput    string        // "" or "2006-01-02"
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

type boardsPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Boards    []model.Board
	Error     string
}

func (s *Server) cardView(u identity.User, b *model.Board, c model.Card) cardView {
	v := cardView{Viewer: u, Card: c, BoardLabels: b.Labels}
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
	p := boardPage{Title: b.Name, User: u, BoardSlug: b.Slug, Board: b, EmptyCard: cardView{Viewer: u, BoardLabels: b.Labels}}
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
	c, err := s.svc.UpdateCard(r.Context(), model.ID(r.PathValue("id")), cardInput(r))
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
	s.render(w, s.parts, "card", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c))
}

func (s *Server) deleteCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteCard(r.Context(), model.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.ready != nil {
		if err := s.ready(ctx); err != nil {
			s.log.Warn("not ready", "err", err)
			plain(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
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
