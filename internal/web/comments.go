// Comments on a card. They cannot be edited, and only their author can take
// one away, which is the whole of the policy.
// Package web serves the HTMX front-end. Handlers parse the request, call
// one service method and render a template; there is no business logic and
// no SQL here.
package web

import (
	"net/http"

	"kanban/internal/identity"
	"kanban/internal/model"
)

// addComment posts one comment and answers with it, plus the card face out of
// band so the count on the board behind the modal keeps up.
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
