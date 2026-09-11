// The board list, one board, and the two writes that change a board itself.
// Package web serves the HTMX front-end. Handlers parse the request, call
// one service method and render a template; there is no business logic and
// no SQL here.
package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
)

// index is the front door. With exactly one board it redirects into it, because
// a list of one is a page nobody wants; with any other number it is the list.
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

// boardList draws every board and the form that makes another.
func (s *Server) boardList(w http.ResponseWriter, r *http.Request) {
	boards, err := s.svc.Boards(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, s.pages["boards"], "layout", http.StatusOK,
		boardsPage{Title: "Boards", User: identity.FromContext(r.Context()), Boards: boards})
}

// navBoards is the list behind the board name in the app bar. A read that fails
// logs and returns nothing: a board that cannot name its neighbours is still a
// board worth drawing, and the switcher simply does not open.
func (s *Server) navBoards(ctx context.Context) []model.Board {
	boards, err := s.svc.Boards(ctx)
	if err != nil {
		s.log.Error("board switcher", "err", err)
		return nil
	}
	return boards
}

// createBoard makes a board from a name and goes to it. A name that is empty
// or already taken re-renders the list with the reason rather than redirecting,
// so what was typed is still on the screen.
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

// deleteBoard removes a board and everything on it.
//
// The only thing in this application with no way back: a card can be archived
// and restored, a column's cards can be moved out from under it, and a board
// takes its cards, its columns, its labels and its comments with it. So the
// name has to be typed, which is the one guard that cannot be satisfied by a
// misplaced click, and the board being deleted is named in the confirmation so
// the name being typed is the one in front of you.
func (s *Server) deleteBoard(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), b.Name) {
		boards, lerr := s.svc.Boards(r.Context())
		if lerr != nil {
			s.fail(w, r, lerr)
			return
		}
		s.render(w, s.pages["boards"], "layout", http.StatusBadRequest, boardsPage{
			Title: "Boards", User: identity.FromContext(r.Context()), Boards: boards,
			Error: "To delete " + b.Name + ", type its name exactly.",
		})
		return
	}
	if err := s.svc.DeleteBoard(r.Context(), b.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	// /boards rather than /, because / redirects straight back into the one
	// board that is left and the person doing this is tidying up.
	http.Redirect(w, r, "/boards", http.StatusSeeOther)
}

// board draws one board: its columns, its cards, and the search results when
// the query string carries a search.
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
		// The hits are a subset, and a quick edit on a result should offer the
		// same people as one on the board, so the list comes from the board.
		people := s.people(r, u, b.ID)
		clock := b.SLA.Clock()
		for _, c := range hits {
			page.Results = append(page.Results, s.cardView(u, b, clock, c, counts[c.ID], people))
		}
		page.Boards = s.navBoards(r.Context())
		s.render(w, s.pages["board"], "layout", http.StatusOK, page)
		return
	}

	cards, err := s.svc.Cards(r.Context(), b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page := s.boardPage(u, b, cards, counts)
	page.Boards = s.navBoards(r.Context())
	s.render(w, s.pages["board"], "layout", http.StatusOK, page)
}

// setLayout switches the board between columns and rows. It posts from the
// board itself rather than the settings page: it is a thing you decide while
// looking at the board, and the answer is visible the moment you land back on
// it.
func (s *Server) setLayout(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetBoardLayout(r.Context(), b.ID, r.FormValue("layout")); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/b/"+b.Slug, http.StatusSeeOther)
}
