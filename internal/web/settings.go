// The board's own settings page: its columns, its labels and its response
// time. Every write here re-renders the page rather than redirecting, so a
// refusal can say what was wrong beside the field that was wrong.

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
)

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

// renderSettings draws the settings page, with an optional message. Every write
// on this page comes back through here, so a refusal arrives on the page that
// asked rather than on a fresh one.
//
// The board is read back from the store, so the fields show what is stored and
// not what was typed: a rename refused for a bad WIP limit comes back with the
// old name in the box and the reason above it. That is worth knowing before
// somebody reports it as the rename being lost, which it is not.
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
	palette := boardPalette(b.Labels)
	p := settingsPage{
		Title:     b.Name + " · settings",
		User:      identity.FromContext(r.Context()),
		BoardSlug: b.Slug,
		Board:     b,
		NewLabel:  labelView{Palette: palette},
		SLA:       slaSettings(b.SLA),
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
			Palette: palette,
		})
	}
	p.Boards = s.navBoards(r.Context())
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

// settingsRedirect sends the browser back to the settings page after a write
// that has nothing to say.
func (s *Server) settingsRedirect(w http.ResponseWriter, r *http.Request, b *model.Board) {
	http.Redirect(w, r, "/b/"+b.Slug+"/settings", http.StatusSeeOther)
}

// createLabel adds a label to the board. A duplicate name is refused here rather
// than quietly merged, because two labels with one name cannot be told apart.
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

// checked reads a checkbox. An unchecked box sends no field at all, so the
// question is whether the field arrived, not what it says; the value a browser
// puts in a ticked one is "on", and nothing should depend on that.
func checked(r *http.Request, name string) bool {
	return r.FormValue(name) != ""
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

// createColumn adds a column at the end of the board.
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
	if _, err := s.svc.AddColumn(r.Context(), b.ID, r.FormValue("name"), limit, checked(r, "stops_clock")); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// updateColumn renames a column and sets its WIP limit and whether it stops the
// response clock.
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
	if err := s.svc.UpdateColumn(r.Context(), col.ID, r.FormValue("name"), limit, checked(r, "stops_clock")); err != nil {
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

// setSLA writes the board's response-time promise. Zero hours switches it off
// and leaves the rest of the form as it was, so turning the clock back on does
// not mean typing the office hours again.
func (s *Server) setSLA(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sla, err := readSLA(r)
	if err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	if err := s.svc.SetBoardSLA(r.Context(), b.ID, sla); err != nil {
		s.settingsFailure(w, r, err)
		return
	}
	s.settingsRedirect(w, r, b)
}

// readSLA reads the response-time form.
func readSLA(r *http.Request) (model.SLA, error) {
	if err := r.ParseForm(); err != nil {
		return model.SLA{}, &service.ValidationError{Field: "form", Message: "could not be read"}
	}
	// The switch decides whether the board promises anything; the number decides
	// how much. An unticked checkbox sends no field at all, which is what makes
	// the switch readable here without a hidden companion field.
	on := r.PostFormValue("on") != ""
	hours, err := strconv.Atoi(orZero(r.PostFormValue("response_hours")))
	if err != nil || hours < 0 {
		return model.SLA{}, &service.ValidationError{
			Field: "response_hours", Message: "must be a whole number of office hours"}
	}
	switch {
	case !on:
		// The hours in the form are kept out of the write: off is off, and a
		// board that promises nothing holds no number to argue about.
		hours = 0
	case hours == 0:
		return model.SLA{}, &service.ValidationError{
			Field: "response_hours", Message: "must be at least one office hour, or switch the response time off"}
	}
	start, err := clockMinutes(r.PostFormValue("start"), false)
	if err != nil {
		return model.SLA{}, err
	}
	end, err := clockMinutes(r.PostFormValue("end"), true)
	if err != nil {
		return model.SLA{}, err
	}
	days, err := model.ParseDays(r.PostForm["days"])
	if err != nil {
		return model.SLA{}, &service.ValidationError{Field: "days", Message: "must be weekdays"}
	}
	return model.SLA{
		ResponseHours: hours,
		Days:          days,
		Start:         start,
		End:           end,
		Zone:          strings.TrimSpace(r.PostFormValue("zone")),
	}, nil
}

// orZero turns an empty number field into a zero, so a cleared box reaches the
// validation as a number rather than failing to parse into one.
//
// It does not switch the clock off. The switch does that, and with the switch
// on, a zero is refused with "must be at least one office hour", which is the
// message somebody clearing the box should get. Turning the promise off by
// emptying a field would be a second way to do what the switch already does,
// with no way to tell it from a slip.
func orZero(v string) string {
	if v = strings.TrimSpace(v); v == "" {
		return "0"
	}
	return v
}

// clockMinutes reads an <input type="time"> as minutes since midnight.
//
// The end of a day arrives as "00:00", because a time input has no 24:00 and
// "open until midnight" has to be sayable. So the same string means 0 at the
// start of the range and 1440 at the end of it, and a desk staffed around the
// clock is 00:00 to 00:00.
func clockMinutes(v string, endOfDay bool) (int, error) {
	m, err := model.ParseClock(v, endOfDay)
	if err != nil {
		return 0, &service.ValidationError{Field: "hours", Message: model.ErrBadClock.Error()}
	}
	return m, nil
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

// updateLabel renames a label or changes its colour, on every card at once.
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

// deleteLabel removes a label from the board and from every card carrying it,
// rather than leaving cards pointing at something that is gone.
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
	// Deleting a label takes it off every card that carried it, so there is no
	// fragment that expresses the change. The quick panel on a card face asks
	// over htmx and gets a reload; the settings page posts a plain form and
	// gets its redirect.
	if isHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.settingsRedirect(w, r, b)
}
