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
	"slices"
	"strconv"
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
	pages   map[string]*template.Template // full pages, keyed by name
	parts   *template.Template            // fragments: card, card_edit
}

// Option configures New.
type Option func(*Server)

// WithClock replaces time.Now, used for the overdue marker.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithBuild records what is running, for /version. Leave it out and the
// endpoint says so rather than making something up.
func WithBuild(version, commit string) Option {
	return func(s *Server) { s.version, s.commit = version, commit }
}

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
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", staticHandler(http.FileServerFS(assets.FS()))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { plain(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", s.buildInfo)
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
		"readableOn":   readableOn,
		// Every asset URL carries the digest of the embedded tree, so a new
		// build is a new URL and the browser cannot serve yesterday's script
		// against today's markup.
		"asset": func(path string) string {
			if v := assets.Version(); v != "" {
				return "/assets/" + path + "?v=" + v
			}
			return "/assets/" + path
		},
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFiles,
		"templates/layout.html", "templates/card.html", "templates/card_edit.html", "templates/comment.html"))
	s.parts = base
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"board", "boards", "archive", "settings"} {
		s.pages[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFiles, "templates/"+name+".html"))
	}
}

// readableOn returns a text colour that can be read on the given background.
//
// Labels used to print white on whatever colour they carried, which is fine on
// the red and the blue and close to invisible on the yellow. The palette holds
// both light and dark entries, so the choice cannot be made once for all of
// them. An unparseable colour gets white, which is what it had before.
func readableOn(background string) string {
	const dark, light = "#111827", "#ffffff"
	r, g, b, ok := parseHex(background)
	if !ok {
		return light
	}
	// Rec. 601 luma. The threshold is where white and near-black are about
	// equally readable; it does not need to be more exact than that.
	if 299*r+587*g+114*b > 150_000 {
		return dark
	}
	return light
}

func parseHex(s string) (r, g, b int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		// #abc is #aabbcc; doubling the digit is the definition, not a guess.
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, true
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
	// DueState is "", "overdue", "today", "soon" or "later". A single flag
	// only distinguished late from not late, which said nothing about a card
	// due in an hour and a card due in a month.
	DueState string
	// SubtasksDone and SubtasksTotal draw the checklist's progress on the card
	// face. Percent is separate because a template cannot divide.
	SubtasksDone    int
	SubtasksTotal   int
	SubtasksPercent int
	// CommentCount draws the badge on the card face. The board loads every
	// card's count in one call; a single-card fragment counts its own.
	CommentCount int
	// Comments is the thread, filled only for the edit form. The board does
	// not read it, because that would be one query per card on every render.
	Comments []commentView
	// OOB marks the card as an out-of-band swap: the answer to a comment
	// carries the card face along so its badge is not left stale behind the
	// open modal. htmx ignores it when the board is not the page on screen.
	OOB bool
}

type commentView struct {
	Comment model.Comment
	// Mine is whether the viewer wrote it, which is who may remove it.
	Mine bool
	// Ago is "3 hours ago"; Exact is the instant, for the title attribute.
	Ago   string
	Exact string
}

type columnView struct {
	Column model.Column
	Cards  []cardView
	Count  int
	// AtLimit is a full column, OverLimit one that is already past. They are
	// separate because the first is working as intended and the second is not,
	// and a column that only turns red once the rule has been broken tells
	// nobody it was about to be.
	AtLimit   bool
	OverLimit bool
	// LimitPercent fills the bar under the header, capped at 100 so an
	// over-full column does not draw outside its own track.
	LimitPercent int
}

type boardPage struct {
	Title string
	User  identity.User
	// Query is what the user typed, echoed back into the box so a reload or a
	// shared link keeps the search.
	Query   string
	Results []cardView
	// Searching is Query being non-empty, not Results being non-empty: a
	// search with no hits must say so rather than silently showing the board.
	Searching bool
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

// labelPalette is what the label form offers.
//
// A board reads better when its labels come from one set of colours than when
// every one is picked by hand, and a fixed list cannot produce a value the
// template has to refuse. The service still accepts any hex colour, so an API
// client is not held to these eight.
var labelPalette = []string{"#ef4444", "#f97316", "#eab308", "#22c55e", "#3b82f6", "#8b5cf6", "#ec4899", "#6b7280"}

type labelView struct {
	Label model.Label
	// Cards is how many cards carry the label, archived ones included.
	// Deleting takes it off all of them, so the page says so first.
	Cards int
	// Custom is a colour that is not one of the presets, set by an API client
	// or an older board. The form offers it as a ninth swatch rather than
	// showing the label as though it had no colour and quietly losing it.
	Custom  bool
	Palette []string
}

type columnSetting struct {
	Column model.Column
	// Cards is how many are in it, the archive included, because deleting the
	// column decides what happens to those as well.
	Cards int
	// Others are where its cards could go instead, on a delete.
	Others []model.Column
	First  bool
	Last   bool
	// Only marks the last column standing. It cannot be deleted, so the form
	// does not offer to.
	Only bool
}

type settingsPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Board     *model.Board
	Columns   []columnSetting
	Labels    []labelView
	NewLabel  labelView
	Error     string
}

func (s *Server) cardView(u identity.User, b *model.Board, c model.Card, comments int) cardView {
	v := cardView{Viewer: u, Card: c, BoardLabels: b.Labels, Columns: b.Columns, CommentCount: comments}
	for _, id := range c.Labels {
		if l := b.Label(id); l != nil {
			v.Labels = append(v.Labels, *l)
		}
	}
	if !c.DueDate.IsZero() {
		v.Due = c.DueDate.Format("2 Jan 2006")
		v.DueInput = c.DueDate.Format("2006-01-02")
		v.DueState = dueState(s.now().UTC().Truncate(24*time.Hour), c.DueDate)
	}
	for _, st := range c.Subtasks {
		if st.Done {
			v.SubtasksDone++
		}
	}
	v.SubtasksTotal = len(c.Subtasks)
	if v.SubtasksTotal > 0 {
		v.SubtasksPercent = v.SubtasksDone * 100 / v.SubtasksTotal
	}
	return v
}

// dueSoon is how far ahead a due date still counts as pressing. Three days is
// long enough to act on and short enough that most of the board is not amber.
const dueSoon = 3

// dueState grades a due date against today. Both are dates at midnight UTC.
func dueState(today, due time.Time) string {
	switch {
	case due.Before(today):
		return "overdue"
	case due.Equal(today):
		return "today"
	case due.Before(today.AddDate(0, 0, dueSoon+1)):
		return "soon"
	default:
		return "later"
	}
}

func (s *Server) commentView(u identity.User, c model.Comment) commentView {
	return commentView{
		Comment: c,
		Mine:    strings.EqualFold(u.Email, c.Author),
		Ago:     ago(s.now().UTC(), c.CreatedAt),
		Exact:   c.CreatedAt.Format("2 Jan 2006, 15:04") + " UTC",
	}
}

// ago renders how long ago something happened.
//
// The server does not know what timezone the reader is in — the app stores and
// shows UTC throughout — so a wall-clock time would be wrong for everyone
// outside it, while "3 hours ago" is right for everybody. The exact instant is
// still there, in the title attribute.
func ago(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	default:
		return then.Format("2 Jan 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func (s *Server) boardPage(u identity.User, b *model.Board, cards []model.Card, comments map[model.ID]int) boardPage {
	p := boardPage{Title: b.Name, User: u, BoardSlug: b.Slug, Board: b, EmptyCard: cardView{Viewer: u, BoardLabels: b.Labels, Columns: b.Columns}}
	byColumn := map[model.ID][]cardView{}
	for _, c := range cards {
		byColumn[c.ColumnID] = append(byColumn[c.ColumnID], s.cardView(u, b, c, comments[c.ID]))
	}
	for _, col := range b.Columns {
		cv := columnView{Column: col, Cards: byColumn[col.ID], Count: len(byColumn[col.ID])}
		if col.WIPLimit > 0 {
			cv.AtLimit = cv.Count == col.WIPLimit
			cv.OverLimit = cv.Count > col.WIPLimit
			cv.LimitPercent = min(cv.Count*100/col.WIPLimit, 100)
		}
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

// fragment is one template and the data to run it with.
type fragment struct {
	name string
	data any
}

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
	u := identity.FromContext(r.Context())
	// One call for the whole board rather than one per card. It covers
	// archived cards too, so a search result carries its badge as well.
	counts, err := s.svc.CommentCounts(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The search lives on the board's own URL rather than a page of its own,
	// so a result set can be linked to and reloading keeps it.
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	if raw != "" {
		hits, err := s.svc.Search(r.Context(), b.ID, service.ParseQuery(raw))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		page := s.boardPage(u, b, nil, counts)
		page.Query, page.Searching = raw, true
		for _, c := range hits {
			page.Results = append(page.Results, s.cardView(u, b, c, counts[c.ID]))
		}
		s.render(w, s.pages["board"], "layout", http.StatusOK, page)
		return
	}

	cards, err := s.svc.Cards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.pages["board"], "layout", http.StatusOK, s.boardPage(u, b, cards, counts))
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
	// A card that was created a microsecond ago has no comments; counting them
	// would be a query that can only ever answer zero.
	s.render(w, s.parts, "card", http.StatusOK, s.cardView(identity.FromContext(r.Context()), b, *c, 0))
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

// cardFragment builds the view for a single-card render. It reads the thread
// rather than taking a count on trust, so a swap cannot silently drop the
// badge from a card that has comments.
func (s *Server) cardFragment(r *http.Request, b *model.Board, c model.Card) (cardView, error) {
	comments, err := s.svc.Comments(r.Context(), c.ID)
	if err != nil {
		return cardView{}, err
	}
	return s.cardView(identity.FromContext(r.Context()), b, c, len(comments)), nil
}

func (s *Server) card(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.cardFragment(r, b, *c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.parts, "card", http.StatusOK, v)
}

func (s *Server) editCard(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	comments, err := s.svc.Comments(r.Context(), c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := identity.FromContext(r.Context())
	v := s.cardView(u, b, *c, len(comments))
	for _, cm := range comments {
		v.Comments = append(v.Comments, s.commentView(u, cm))
	}
	s.render(w, s.parts, "card_edit", http.StatusOK, v)
}

// refreshedCard builds the card face as an out-of-band fragment, so that a
// change made inside the modal is reflected on the board behind it. An error
// here is not fatal to the request that caused it: the comment was written,
// and a stale badge is a worse answer than a 500 only if it is silent, so it
// is logged.
func (s *Server) refreshedCard(r *http.Request, cardID model.ID) (fragment, bool) {
	v, err := s.cardViewByID(r, cardID)
	if err != nil {
		s.log.Warn("could not refresh the card face", "card", cardID, "err", err)
		return fragment{}, false
	}
	v.OOB = true
	return fragment{"card", v}, true
}

func (s *Server) cardViewByID(r *http.Request, cardID model.ID) (cardView, error) {
	c, err := s.svc.Card(r.Context(), cardID)
	if err != nil {
		return cardView{}, err
	}
	b, err := s.svc.BoardByID(r.Context(), c.BoardID)
	if err != nil {
		return cardView{}, err
	}
	return s.cardFragment(r, b, *c)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	u := identity.FromContext(r.Context())
	cardID := model.ID(r.PathValue("id"))
	c, err := s.svc.AddComment(r.Context(), cardID, u.Email, r.FormValue("body"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	frags := []fragment{{"comment", s.commentView(u, *c)}}
	if card, ok := s.refreshedCard(r, cardID); ok {
		frags = append(frags, card)
	}
	s.renderAll(w, s.parts, http.StatusOK, frags...)
}

// deleteComment answers with nothing but the refreshed card face. The button
// targets the comment with hx-swap="outerHTML", and htmx lifts the card out of
// the body as an out-of-band swap first, so what is left to replace the
// comment with is the empty string.
func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	u := identity.FromContext(r.Context())
	c, err := s.svc.DeleteComment(r.Context(), model.ID(r.PathValue("id")), u.Email)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if card, ok := s.refreshedCard(r, c.CardID); ok {
		s.renderAll(w, s.parts, http.StatusOK, card)
		return
	}
	w.WriteHeader(http.StatusOK)
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
	v, err := s.cardFragment(r, b, *c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.parts, "card", http.StatusOK, v)
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
	counts, err := s.svc.CommentCounts(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := identity.FromContext(r.Context())
	page := archivePage{Title: b.Name + " · archive", User: u, BoardSlug: b.Slug, Board: b}
	for _, c := range cards {
		page.Cards = append(page.Cards, s.cardView(u, b, c, counts[c.ID]))
	}
	s.render(w, s.pages["archive"], "layout", http.StatusOK, page)
}

// --- settings -----------------------------------------------------------------

// settings renders the board's columns and labels. Neither could be edited
// without writing Go before this: the service methods were all there and had
// no way in, which is why a WIP limit was a thing the model knew about and
// nobody could set.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "")
}

// settingsMoved keeps the /labels URL working. It shipped, and a bookmark to
// it should land on the page that replaced it rather than on a 404.
func (s *Server) settingsMoved(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/b/"+r.PathValue("board")+"/settings", http.StatusMovedPermanently)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, message string) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	labelUse, columnUse, err := s.boardUse(r, b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := settingsPage{
		Title:     b.Name + " · settings",
		User:      identity.FromContext(r.Context()),
		BoardSlug: b.Slug,
		Board:     b,
		NewLabel:  labelView{Palette: labelPalette},
		Error:     message,
	}
	for i, col := range b.Columns {
		cs := columnSetting{
			Column: col,
			Cards:  columnUse[col.ID],
			First:  i == 0,
			Last:   i == len(b.Columns)-1,
			Only:   len(b.Columns) == 1,
		}
		for _, other := range b.Columns {
			if other.ID != col.ID {
				cs.Others = append(cs.Others, other)
			}
		}
		p.Columns = append(p.Columns, cs)
	}
	for _, l := range b.Labels {
		p.Labels = append(p.Labels, labelView{
			Label:   l,
			Cards:   labelUse[l.ID],
			Custom:  l.Color != "" && !slices.Contains(labelPalette, l.Color),
			Palette: labelPalette,
		})
	}
	s.render(w, s.pages["settings"], "layout", status, p)
}

// boardUse counts what each label and each column carries, the archive
// included: a label only archived cards wear is still in use, and deleting the
// column they sit in decides what happens to them too. Both counts come from
// the same two reads.
func (s *Server) boardUse(r *http.Request, boardID model.ID) (labels, columns map[model.ID]int, err error) {
	on, err := s.svc.Cards(r.Context(), boardID)
	if err != nil {
		return nil, nil, err
	}
	off, err := s.svc.ArchivedCards(r.Context(), boardID)
	if err != nil {
		return nil, nil, err
	}
	labels, columns = map[model.ID]int{}, map[model.ID]int{}
	for _, list := range [][]model.Card{on, off} {
		for _, c := range list {
			columns[c.ColumnID]++
			for _, id := range c.Labels {
				labels[id]++
			}
		}
	}
	return labels, columns, nil
}

// settingsFailure puts a rejected edit back on the page with the reason,
// rather than answering with a bare status the form has nowhere to show.
func (s *Server) settingsFailure(w http.ResponseWriter, r *http.Request, err error) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		s.renderSettings(w, r, http.StatusBadRequest, ve.Message)
	case errors.Is(err, store.ErrConflict):
		// Only labels are unique by name; two columns may share one.
		s.renderSettings(w, r, http.StatusConflict, "this board already has a label with that name")
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) settingsRedirect(w http.ResponseWriter, r *http.Request, b *model.Board) {
	http.Redirect(w, r, "/b/"+b.Slug+"/settings", http.StatusSeeOther)
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.svc.CreateLabel(r.Context(), b.ID, r.FormValue("name"), r.FormValue("color")); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// --- columns ------------------------------------------------------------------

// wipLimit reads the limit field. Empty is no limit, which is what the model
// stores as zero.
func wipLimit(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, &service.ValidationError{Field: "wip_limit", Message: "must be a whole number, or empty for no limit"}
	}
	return n, nil
}

// boardColumn resolves the column id in the path against the board in the
// path, so a crafted id cannot reach a column on another board.
func (s *Server) boardColumn(r *http.Request) (*model.Board, *model.Column, error) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		return nil, nil, err
	}
	c := b.Column(model.ID(r.PathValue("id")))
	if c == nil {
		return nil, nil, store.ErrNotFound
	}
	return b, c, nil
}

func (s *Server) createColumn(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit, err := wipLimit(r.FormValue("wip_limit"))
	if err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	if _, err := s.svc.AddColumn(r.Context(), b.ID, r.FormValue("name"), limit); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

func (s *Server) updateColumn(w http.ResponseWriter, r *http.Request) {
	b, col, err := s.boardColumn(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit, err := wipLimit(r.FormValue("wip_limit"))
	if err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	if err := s.svc.UpdateColumn(r.Context(), col.ID, r.FormValue("name"), limit); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// deleteColumn removes a column. move_to names where its cards go; empty means
// they go with it, which is why the form makes that the deliberate choice
// rather than the default.
func (s *Server) deleteColumn(w http.ResponseWriter, r *http.Request) {
	b, col, err := s.boardColumn(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.RemoveColumn(r.Context(), b.ID, col.ID, model.ID(r.FormValue("move_to"))); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// moveColumn swaps a column with its neighbour. A move off either end is a
// no-op rather than an error: the buttons are not offered there, and a repeated
// submit should not be an error page.
func (s *Server) moveColumn(w http.ResponseWriter, r *http.Request) {
	b, col, err := s.boardColumn(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	order := make([]model.ID, len(b.Columns))
	at := -1
	for i, c := range b.Columns {
		order[i] = c.ID
		if c.ID == col.ID {
			at = i
		}
	}
	to := at - 1
	if r.FormValue("direction") == "down" {
		to = at + 1
	}
	if to < 0 || to >= len(order) {
		s.settingsRedirect(w, r, b)
		return
	}
	order[at], order[to] = order[to], order[at]
	if err := s.svc.ReorderColumns(r.Context(), b.ID, order); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// boardLabel resolves the label id in the path against the board in the path,
// so a crafted id cannot reach a label that belongs to another board.
func (s *Server) boardLabel(r *http.Request) (*model.Board, *model.Label, error) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		return nil, nil, err
	}
	l := b.Label(model.ID(r.PathValue("id")))
	if l == nil {
		return nil, nil, store.ErrNotFound
	}
	return b, l, nil
}

func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request) {
	b, l, err := s.boardLabel(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.UpdateLabel(r.Context(), l.ID, r.FormValue("name"), r.FormValue("color")); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	b, l, err := s.boardLabel(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteLabel(r.Context(), l.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

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
