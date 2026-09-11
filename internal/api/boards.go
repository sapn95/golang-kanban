package api

import (
	"net/http"

	"kanban/internal/model"
)

// A board is addressed by slug, the way a page is, so a URL a person can read
// off the address bar is the URL a script uses. Columns and labels are
// addressed inside their board rather than by id alone: their service methods
// mostly take an id without checking which board it belongs to, and this is
// where that is checked, so an id from another board is a 404 and not a write.
// RemoveColumn is the one that checks for itself, because where the cards go is
// a second id that has to be on the same board.

// listBoards answers GET /boards with every board and its columns and labels.
func (s *Server) listBoards(w http.ResponseWriter, r *http.Request) {
	boards, err := s.svc.Boards(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toBoards(boards))
}

// createBoard answers POST /boards. The slug is optional: without one the
// service makes it from the name.
func (s *Server) createBoard(w http.ResponseWriter, r *http.Request) {
	var in boardInput
	if !s.decode(w, r, &in) {
		return
	}
	b, err := s.svc.CreateBoard(r.Context(), in.Name, in.Slug, in.Columns)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/boards/"+b.Slug)
	s.write(w, r, http.StatusCreated, toBoard(*b))
}

// getBoard answers GET /boards/{board}.
func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	s.write(w, r, http.StatusOK, toBoard(*b))
}

// renameBoard changes the name, the slug, or both. What is left out keeps its
// current value, which is why the request is read against the board rather than
// straight into the service call.
func (s *Server) renameBoard(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in boardPatch
	if !s.decode(w, r, &in) {
		return
	}
	name, slug := b.Name, b.Slug
	if in.Name != nil {
		name = *in.Name
	}
	if in.Slug != nil {
		slug = *in.Slug
	}
	renamed, err := s.svc.RenameBoard(r.Context(), b.ID, name, slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, toBoard(*renamed))
}

// deleteBoard answers DELETE /boards/{board}, taking its cards, columns, labels
// and comments with it.
func (s *Server) deleteBoard(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	if err := s.svc.DeleteBoard(r.Context(), b.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setLayout answers PUT /boards/{board}/layout with columns or rows.
func (s *Server) setLayout(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in layoutInput
	if !s.decode(w, r, &in) {
		return
	}
	if err := s.svc.SetBoardLayout(r.Context(), b.ID, in.Layout); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setSLA replaces the whole promise. A PUT with response_hours 0 is how a board
// stops making one, which is also what the settings form posts when the hours
// are cleared.
func (s *Server) setSLA(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in slaInput
	if !s.decode(w, r, &in) {
		return
	}
	sla, err := in.toModel()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetBoardSLA(r.Context(), b.ID, sla); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- columns ------------------------------------------------------------------

// createColumn answers POST /boards/{board}/columns, appending at the end.
func (s *Server) createColumn(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in columnInput
	if !s.decode(w, r, &in) {
		return
	}
	c, err := s.svc.AddColumn(r.Context(), b.ID, in.Name, in.WIPLimit, in.StopsClock)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusCreated, toColumn(*c))
}

// updateColumn answers PATCH on one column. Absent fields are left alone, which
// is what makes it a PATCH rather than a PUT.
func (s *Server) updateColumn(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	id, ok := s.columnOf(w, r, b)
	if !ok {
		return
	}
	var in columnPatch
	if !s.decode(w, r, &in) {
		return
	}
	// What is not in the body keeps the value it has. The column is read back
	// from the board we already loaded, so this costs nothing.
	col := b.Column(id)
	name, limit, stops := col.Name, col.WIPLimit, col.StopsClock
	if in.Name != nil {
		name = *in.Name
	}
	if in.WIPLimit != nil {
		limit = *in.WIPLimit
	}
	if in.StopsClock != nil {
		stops = *in.StopsClock
	}
	if err := s.svc.UpdateColumn(r.Context(), id, name, limit, stops); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteColumn takes ?move_to= a column of the same board that inherits the
// cards. Without it the cards go with the column, which is why it is a
// parameter and not a default: deleting a lane full of work should be something
// somebody asked for.
func (s *Server) deleteColumn(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	id, ok := s.columnOf(w, r, b)
	if !ok {
		return
	}
	moveTo := model.ID(r.URL.Query().Get("move_to"))
	if err := s.svc.RemoveColumn(r.Context(), b.ID, id, moveTo); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reorderColumns takes the whole order, unlike the settings page, which moves
// one column one place. The service method is the whole order either way, and
// a caller with a program in hand can say what it wants.
func (s *Server) reorderColumns(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in orderInput
	if !s.decode(w, r, &in) {
		return
	}
	if err := s.svc.ReorderColumns(r.Context(), b.ID, toIDs(in.Order)); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- labels -------------------------------------------------------------------

// createLabel answers POST /boards/{board}/labels.
func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	var in labelInput
	if !s.decode(w, r, &in) {
		return
	}
	l, err := s.svc.CreateLabel(r.Context(), b.ID, in.Name, in.Color)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusCreated, toLabel(*l))
}

// updateLabel answers PATCH on one label, leaving absent fields alone.
func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	id, ok := s.labelOf(w, r, b)
	if !ok {
		return
	}
	var in labelPatch
	if !s.decode(w, r, &in) {
		return
	}
	// As for a column: an absent field keeps what the label has, so renaming one
	// does not send its colour back to the default.
	lab := b.Label(id)
	name, color := lab.Name, lab.Color
	if in.Name != nil {
		name = *in.Name
	}
	if in.Color != nil {
		color = *in.Color
	}
	if err := s.svc.UpdateLabel(r.Context(), id, name, color); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteLabel takes the label off every card that carries it, which is the
// service method's own promise and worth knowing before calling it.
func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	b, ok := s.board(w, r)
	if !ok {
		return
	}
	id, ok := s.labelOf(w, r, b)
	if !ok {
		return
	}
	if err := s.svc.DeleteLabel(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- the two id checks --------------------------------------------------------

// columnOf reads the column named in the path and checks it is on this board. A
// column of another board is a 404 here rather than a write to somebody else's.
func (s *Server) columnOf(w http.ResponseWriter, r *http.Request, b *model.Board) (model.ID, bool) {
	id := model.ID(r.PathValue("column"))
	if b.Column(id) == nil {
		s.error(w, r, http.StatusNotFound, "no such column on this board")
		return "", false
	}
	return id, true
}

// labelOf does the same for a label.
func (s *Server) labelOf(w http.ResponseWriter, r *http.Request, b *model.Board) (model.ID, bool) {
	id := model.ID(r.PathValue("label"))
	if b.Label(id) == nil {
		s.error(w, r, http.StatusNotFound, "no such label on this board")
		return "", false
	}
	return id, true
}
