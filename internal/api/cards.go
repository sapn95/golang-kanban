package api

import (
	"net/http"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
)

// A card is addressed by id, not through its board, because a card id is what a
// caller has after creating one and what the page URLs use. The service checks
// that a label and a column named on a card belong to that card's own board, so
// a card route needs no board in the path to be safe.

func (s *Server) listCards(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	// ?q= is the same search the board page runs, syntax included, so a saved
	// search in a browser and one in a script are the same string.
	if q := r.URL.Query().Get("q"); q != "" {
		hits, err := s.svc.Search(r.Context(), b.ID, service.ParseQuery(q))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.write(w, r, http.StatusOK, toCards(hits))
		return
	}
	cards, err := s.svc.Cards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCards(cards))
}

func (s *Server) listArchived(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	cards, err := s.svc.ArchivedCards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCards(cards))
}

func (s *Server) createCard(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in cardInput
	if !s.decode(w, r, &in) {
		return
	}
	// The first column, for the same reason the page does it: a script that
	// files a card without caring where it lands should not have to read the
	// board first to find out what the columns are called.
	column := model.ID(in.ColumnID)
	if column == "" && len(b.Columns) > 0 {
		column = b.Columns[0].ID
	}
	assignee, ok := s.assignee(w, r, in.Assignee)
	if !ok {
		return
	}
	in.Assignee = assignee

	c, err := s.svc.CreateCard(r.Context(), b.ID, column, in.toService())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/cards/"+string(c.ID))
	s.write(w, r, http.StatusCreated, toCard(*c))
}

func (s *Server) getCard(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.Card(r.Context(), model.ID(r.PathValue("card")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCard(*c))
}

// updateCard replaces the card's content, and moves it first when the request
// names another column.
//
// The order matters and it is the order the edit form uses. A move is a reorder
// of the destination column and a WIP limit can refuse it, so doing it first
// means a refused move leaves the card as it was rather than saving a new title
// into a card that did not go anywhere.
func (s *Server) updateCard(w http.ResponseWriter, r *http.Request) {
	id := model.ID(r.PathValue("card"))
	var in cardInput
	if !s.decode(w, r, &in) {
		return
	}
	assignee, ok := s.assignee(w, r, in.Assignee)
	if !ok {
		return
	}
	in.Assignee = assignee

	if want := model.ID(in.ColumnID); want != "" {
		current, err := s.svc.Card(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if current.ColumnID != want {
			if err := s.svc.ReorderCards(r.Context(), current.BoardID, want, []model.ID{id}); err != nil {
				s.fail(w, r, err)
				return
			}
		}
	}
	c, err := s.svc.UpdateCard(r.Context(), id, in.toService())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCard(*c))
}

func (s *Server) setAssignee(w http.ResponseWriter, r *http.Request) {
	var in assigneeInput
	if !s.decode(w, r, &in) {
		return
	}
	assignee, ok := s.assignee(w, r, in.Assignee)
	if !ok {
		return
	}
	c, err := s.svc.SetCardAssignee(r.Context(), model.ID(r.PathValue("card")), assignee)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCard(*c))
}

// toggleLabel puts a label on a card or takes it off, whichever the card is
// not. Which of the two it did is in the card that comes back: an "add" that
// answered 409 for a label the card already carries would make a script that
// wants a label on a card check first for no reason.
func (s *Server) toggleLabel(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.ToggleCardLabel(r.Context(),
		model.ID(r.PathValue("card")), model.ID(r.PathValue("label")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toCard(*c))
}

func (s *Server) archiveCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ArchiveCard(r.Context(), model.ID(r.PathValue("card"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RestoreCard(r.Context(), model.ID(r.PathValue("card"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCard(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteCard(r.Context(), model.ID(r.PathValue("card"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bulkCards is one action over a set of cards. It answers 200 with what
// changed and what did not, including when nothing changed: the operations are
// independent, so a card somebody else deleted a second ago belongs in the
// report rather than in a status code that hides the other forty-nine.
func (s *Server) bulkCards(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in bulkInput
	if !s.decode(w, r, &in) {
		return
	}
	target := in.Target
	if service.BulkAction(in.Action) == service.BulkAssign {
		if target, ok = s.assignee(w, r, in.Target); !ok {
			return
		}
	}
	res, err := s.svc.Bulk(r.Context(), b.ID, service.BulkAction(in.Action), toIDs(in.IDs), target)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body := bulkBody{Changed: make([]string, 0, len(res.Changed)), Failed: map[string]string{}}
	for _, id := range res.Changed {
		body.Changed = append(body.Changed, string(id))
	}
	for id, err := range res.Failed {
		_, message, _ := statusFor(err)
		body.Failed[string(id)] = message
	}
	if len(res.Failed) > 0 {
		s.log.Warn("bulk action partially failed", "action", in.Action,
			"changed", len(res.Changed), "failed", len(res.Failed))
	}
	s.write(w, r, http.StatusOK, body)
}

// reorderCards sets the order of one column, and moves any card named in the
// order that is currently somewhere else on the board. That is the drag and
// drop the board page does, in one call: a card lands in a column at a
// position, and the WIP limit of the destination can refuse it.
//
// The order is the whole column. A card of that column left out of the list
// keeps its place behind the ones named, which is what makes moving a single
// card a one-element request.
func (s *Server) reorderCards(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	column, ok := s.columnOf(w, r, b)
	if !ok {
		return
	}
	var in orderInput
	if !s.decode(w, r, &in) {
		return
	}
	if err := s.svc.ReorderCards(r.Context(), b.ID, column, toIDs(in.Order)); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- comments -----------------------------------------------------------------

// listComments reads the card first, so a thread asked for on an id that does
// not exist is a 404 rather than an empty list. An empty list means a card
// nobody has written on, and the two should not look the same.
func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	id := model.ID(r.PathValue("card"))
	if _, err := s.svc.Card(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	comments, err := s.svc.Comments(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toComments(comments))
}

// addComment writes the caller's own address as the author. There is no author
// field in the request: an API that let one be sent would let a script sign
// somebody else's name to a comment nobody can edit afterwards.
func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	var in commentInput
	if !s.decode(w, r, &in) {
		return
	}
	u := identity.FromContext(r.Context())
	c, err := s.svc.AddComment(r.Context(), model.ID(r.PathValue("card")), u.Email, in.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusCreated, toComment(*c))
}

// deleteComment refuses anybody but the author with a 403. Where the
// deployment has no authentication the author is the empty address and so is
// the caller, which matches, and a board with no identity has one user by
// definition.
func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	u := identity.FromContext(r.Context())
	if _, err := s.svc.DeleteComment(r.Context(), model.ID(r.PathValue("comment")), u.Email); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
