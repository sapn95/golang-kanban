// Everything a card can have done to it from a page: made, edited a field at
// a time, moved, archived, restored, deleted.

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
)

// cardInput reads the fields a card form posts. Validation belongs to the
// service, so this only gathers; what it gathers is refused there.
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

// createCard adds a card to a column and answers with the card, retargeted so
// htmx lands it in the right list.
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
	// The Add Card button is on the board page whether or not a search is
	// showing, and a search replaces the columns with a grid of hits. So the
	// list this card belongs in is not on the screen, and neither are the
	// headers: retargeting at it would drop the card into nothing and the
	// person who typed it would watch it vanish. A refresh puts them back on
	// the board with the card on it.
	if !hasColumnHeads(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("HX-Retarget", "#cards-"+string(c.ColumnID))
	w.Header().Set("HX-Reswap", "beforeend")
	// A card that was created a microsecond ago has no comments; counting them
	// would be a query that can only ever answer zero.
	u := identity.FromContext(r.Context())
	card := fragment{"card", s.cardView(u, b, b.SLA.Clock(), *c, 0, s.people(r, u, b.ID))}
	s.renderAll(w, s.parts, http.StatusOK, append([]fragment{card}, s.columnHeads(r.Context(), b)...)...)
}

// subtaskRow is one empty line of a checklist, for the Add Subtask button. It
// takes nothing and reads nothing: the row is markup, and the only reason it
// comes from here is so that the markup of a checklist line lives in one file.
func (s *Server) subtaskRow(w http.ResponseWriter, _ *http.Request) {
	s.render(w, s.parts, "subtaskrow", http.StatusOK, model.Subtask{})
}

// reorderCards takes the whole order of one column as repeated `order` fields,
// the same encoding every other write on these pages uses. It answers with the
// board's column headers, so the count and the WIP bar on both the column the
// card left and the one it landed in are redrawn by the server.
func (s *Server) reorderCards(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	order := make([]model.ID, 0, len(r.Form["order"]))
	for _, id := range r.Form["order"] {
		order = append(order, model.ID(id))
	}
	if err := s.svc.ReorderCards(r.Context(), b.ID, model.ID(r.PathValue("column")), order); err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderAll(w, s.parts, http.StatusOK, s.columnHeads(r.Context(), b)...)
}

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
		if !u.Person() {
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
		// The page reloads either way, so without this a move that a WIP limit
		// refused looked exactly like one that worked: the selection cleared,
		// the board came back, and three of the five cards were where they
		// started with nothing on screen saying why.
		w.Header().Set("HX-Trigger", bulkFailureTrigger(action, res))
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// bulkFailureTrigger builds the HX-Trigger that tells the page how many cards
// the action did not touch. A JSON value rather than a bare event name, because
// the count is the whole message: "3 of 5" is the difference between a limit
// doing its job and a board that ignored the button.
func bulkFailureTrigger(action service.BulkAction, res service.BulkResult) string {
	msg := fmt.Sprintf("%d of %d cards could not be %s.",
		len(res.Failed), len(res.Failed)+len(res.Changed), bulkVerbs[action])
	b, err := json.Marshal(map[string]map[string]string{"kanban:bulk-partial": {"message": msg}})
	if err != nil {
		// The only values here are a short string we built, so this cannot
		// happen; an event with no detail still beats silence.
		return "kanban:bulk-partial"
	}
	return string(b)
}

// bulkVerbs reads each action back as the past participle the message needs.
var bulkVerbs = map[service.BulkAction]string{
	service.BulkMove:    "moved",
	service.BulkAssign:  "assigned",
	service.BulkArchive: "archived",
	service.BulkDelete:  "deleted",
}

// cardAndBoard reads the card named in the path together with the board it is
// on, for the handlers that need the board to redraw its column headers.
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
	u := identity.FromContext(r.Context())
	return s.cardView(u, b, b.SLA.Clock(), c, len(comments), s.people(r, u, b.ID)), nil
}

// people is the list a quick-edit menu offers, for the handlers that do not
// already hold the board's cards.
//
// A failure is logged and answered with just the viewer rather than failing the
// render. The menu is an offer: a card drawn with a short menu is a better
// answer than a card that does not draw.
func (s *Server) people(r *http.Request, u identity.User, boardID model.ID) []string {
	cards, err := s.svc.Cards(r.Context(), boardID)
	if err != nil {
		s.log.Warn("could not list a board's people", "board", boardID, "err", err)
		return service.People(nil, u.Email)
	}
	return service.People(cards, u.Email)
}

// card answers with one card's face, which is what a swap asks for when
// something else on the page has changed it.
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

// editCard answers with the edit form and the card's comments.
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
	// No people list: the modal renders the full form, which has an assignee
	// field of its own, and never the card face.
	v := s.cardView(u, b, b.SLA.Clock(), *c, len(comments), nil)
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

// cardViewByID builds a card's face from its id alone, for the handlers that
// have changed something else and want the card redrawn along with it.
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

// updateCard saves the edit form. A changed column answers 204 and asks for a
// refresh, because the card now belongs to a list this response cannot reach.
func (s *Server) updateCard(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	id := model.ID(r.PathValue("id"))

	// Checked before anything is written. The column moves first, because the
	// store's UpdateCard deliberately never touches ColumnID, so a move is a
	// reorder and a reorder is what a WIP limit refuses; doing it first means a
	// refused move leaves the card exactly as it was. Which left the other
	// order wrong: a move that worked followed by a title that is refused
	// answered 400 with the card already in the other column. Both writes hang
	// on this one answer now.
	in := cardInput(r)
	if err := s.svc.CheckCardInput(in); err != nil {
		s.fail(w, r, err)
		return
	}

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

	c, err := s.svc.UpdateCard(r.Context(), id, in)
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

// setCardAssignee is the quick edit on the card face. It sends one field, so
// two people reassigning cards on the same board at the same time cannot
// overwrite each other's titles the way the full form would.
func (s *Server) setCardAssignee(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	assignee := r.FormValue("assignee")
	if assignee == "@me" {
		// The same reason the bulk toolbar uses this: the viewer's own address
		// is not written into the page, so the button asks for it by name.
		u := identity.FromContext(r.Context())
		if !u.Person() {
			plain(w, http.StatusForbidden, "not signed in")
			return
		}
		assignee = u.Email
	}
	c, err := s.svc.SetCardAssignee(r.Context(), model.ID(r.PathValue("id")), assignee)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.quickEdited(w, r, c)
}

// setCardDue is the date picker on the card face: double-clicking the due chip
// opens an <input type="date">, and this is what it posts to. An empty field
// takes the date off, which is what the Clear button beside it sends.
func (s *Server) setCardDue(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		plain(w, http.StatusBadRequest, "bad form")
		return
	}
	c, err := s.svc.SetCardDueDate(r.Context(), model.ID(r.PathValue("id")), r.FormValue("due_date"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.quickEdited(w, r, c)
}

// toggleSubtask ticks one checklist line from the board, without the edit form.
//
// The checklist was readable on the card and editable only inside a modal, which
// on a phone is a tap, a wait, a scroll, a tick, a save and a close to record
// something that takes a second to do. It is the same shape as the label chip
// beside it: post, and take the card back.
func (s *Server) toggleSubtask(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.ToggleSubtask(r.Context(),
		model.ID(r.PathValue("id")), model.ID(r.PathValue("subtask")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.quickEdited(w, r, c)
}

// toggleCardLabel puts one of the board's labels on a card, or takes it off.
// Which of the two it is comes from the card, not from the request: a button
// that said "add" would be wrong the moment somebody else clicked first. The
// label stays on the board either way; the bin on the settings page removes one.
func (s *Server) toggleCardLabel(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.ToggleCardLabel(r.Context(),
		model.ID(r.PathValue("id")), model.ID(r.PathValue("label")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.quickEdited(w, r, c)
}

// quickEdited answers a quick edit with the card face redrawn in place. The
// menu is not sent back with it: a fresh card comes with its panels closed,
// which is what a click on a menu item should leave behind.
func (s *Server) quickEdited(w http.ResponseWriter, r *http.Request, c *model.Card) {
	b, err := s.svc.BoardByID(r.Context(), c.BoardID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
		return
	}
	v, err := s.cardFragment(r, b, *c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.parts, "card", http.StatusOK, v)
}

// deleteCard removes the card for good. The row goes with hx-swap="delete" on
// the button, and the response body is the column headers, which htmx takes out
// of it and puts back on the board.
//
// Not from the archive, though. The headers are out-of-band swaps aimed at
// elements that exist on the board page and nowhere else, so sending them to
// the archive logged one "no target" per column in the console on every delete.
// The archive draws no counts, so it has nothing to bring up to date.
func (s *Server) deleteCard(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteCard(r.Context(), c.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	if !hasColumnHeads(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.renderAll(w, s.parts, http.StatusOK, s.columnHeads(r.Context(), b)...)
}

// hasColumnHeads reports whether the page a write came from is drawing the
// board's column headers, and so has somewhere to put them when they come back
// out of band.
//
// Two pages do not. The archive is a flat list with no columns, and a board
// showing search results replaces the columns with a grid of hits. Both post to
// the same routes from the same kind of row, so the only thing that tells them
// apart is the URL htmx sends along.
//
// Sending a header to a page that has no element for it is one console error
// per column, on every delete. Harmless, and the kind of noise that trains
// somebody to stop reading the console.
func hasColumnHeads(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		// No URL to judge by: send them. A missing target is noise, and a
		// header that never arrived is a count that stays wrong.
		return true
	}
	if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/archive") {
		return false
	}
	return u.Query().Get("q") == ""
}

// archiveCard takes a card off the board. The card row is removed from the
// page, exactly as a delete does, because from the board's point of view the
// two look the same; the difference is that this one can be undone.
func (s *Server) archiveCard(w http.ResponseWriter, r *http.Request) {
	c, b, err := s.cardAndBoard(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.ArchiveCard(r.Context(), c.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	if !hasColumnHeads(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.renderAll(w, s.parts, http.StatusOK, s.columnHeads(r.Context(), b)...)
}

// restoreCard brings an archived card back to the board it came from, into the
// column it was in.
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

// archive draws the archived cards of one board, searchable like the board is.
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The board's search, on the board's syntax, scoped to the archive whether
	// or not the query says so: this page is the archive, and a search made here
	// that came back with live cards would be answering a different question.
	// An empty q is the whole archive, which is what the page showed before.
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	q := service.ParseQuery(raw)
	q.Archived = true
	cards, err := s.svc.Search(r.Context(), b.ID, q)
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
	page := archivePage{Title: b.Name + " · archive", User: u, BoardSlug: b.Slug, Board: b,
		Query: raw, Searching: raw != ""}
	// Resolved once for the page, though an archived card is never graded: the
	// clock stops when a card leaves the board.
	clock := b.SLA.Clock()
	for _, c := range cards {
		// No people list: the archive draws its own rows, and the action there
		// is to restore a card rather than to reassign it.
		page.Cards = append(page.Cards, s.cardView(u, b, clock, c, counts[c.ID], nil))
	}
	page.Boards = s.navBoards(r.Context())
	s.render(w, s.pages["archive"], "layout", http.StatusOK, page)
}
