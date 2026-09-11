// Package api serves the JSON API. It calls the same service methods the HTML
// front-end calls, so the two cannot drift apart: there is no business rule and
// no SQL in here either, only a request parsed into arguments and a result
// turned into JSON.
//
// What it is for is scripting a board — a nightly card from a cron job, a
// report over a shell pipeline, the seeding of a fresh install — which is work
// that today means posting form bodies to the page routes and reading HTML back.
//
// The paths are versioned and the page routes are not, on purpose. A page URL
// is the page's own vocabulary and renaming one breaks a bookmark; a caller of
// /api/v1 has a program in the way that nobody is going to fix by hand, so the
// prefix is a promise that within v1 a field is not renamed and a status code is
// not changed. See docs/adr/0009-json-api.md.
//
// There are no tokens. The API grants exactly what the board grants, which is
// everything to whoever can reach the port, so a deployment protects it the way
// it protects the pages: with the proxy or Cloudflare Access in front. Identity
// is read from the same middleware and used for the same two things, an
// assignment and a comment author.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
)

// Prefix is where every route lives. The server mounts the handler here, so
// the two spellings cannot drift.
const Prefix = "/api/v1/"

// maxBody caps a request body when this handler is used on its own. Mounted in
// the usual server the same cap has already been applied a layer above;
// applying it twice costs nothing and means the package is safe by itself.
const maxBody = 1 << 20

// Server holds the handlers.
type Server struct {
	svc *service.Kanban
	log *slog.Logger
}

// New builds the JSON API handler. Every route it answers starts with Prefix,
// so it can be mounted on a mux that serves the pages.
func New(svc *service.Kanban, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{svc: svc, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/{$}", s.index)
	mux.HandleFunc("GET /api/v1/openapi.json", s.openAPI)

	mux.HandleFunc("GET /api/v1/boards", s.listBoards)
	mux.HandleFunc("POST /api/v1/boards", s.createBoard)
	mux.HandleFunc("GET /api/v1/boards/{board}", s.getBoard)
	mux.HandleFunc("PATCH /api/v1/boards/{board}", s.renameBoard)
	mux.HandleFunc("DELETE /api/v1/boards/{board}", s.deleteBoard)
	mux.HandleFunc("PUT /api/v1/boards/{board}/layout", s.setLayout)
	mux.HandleFunc("PUT /api/v1/boards/{board}/sla", s.setSLA)

	mux.HandleFunc("POST /api/v1/boards/{board}/columns", s.createColumn)
	mux.HandleFunc("PUT /api/v1/boards/{board}/columns/order", s.reorderColumns)
	mux.HandleFunc("PATCH /api/v1/boards/{board}/columns/{column}", s.updateColumn)
	mux.HandleFunc("DELETE /api/v1/boards/{board}/columns/{column}", s.deleteColumn)
	mux.HandleFunc("PUT /api/v1/boards/{board}/columns/{column}/cards/order", s.reorderCards)

	mux.HandleFunc("POST /api/v1/boards/{board}/labels", s.createLabel)
	mux.HandleFunc("PATCH /api/v1/boards/{board}/labels/{label}", s.updateLabel)
	mux.HandleFunc("DELETE /api/v1/boards/{board}/labels/{label}", s.deleteLabel)

	mux.HandleFunc("GET /api/v1/boards/{board}/cards", s.listCards)
	mux.HandleFunc("POST /api/v1/boards/{board}/cards", s.createCard)
	mux.HandleFunc("POST /api/v1/boards/{board}/cards/bulk", s.bulkCards)
	mux.HandleFunc("GET /api/v1/boards/{board}/archive", s.listArchived)

	mux.HandleFunc("GET /api/v1/cards/{card}", s.getCard)
	mux.HandleFunc("PUT /api/v1/cards/{card}", s.updateCard)
	mux.HandleFunc("DELETE /api/v1/cards/{card}", s.deleteCard)
	mux.HandleFunc("PUT /api/v1/cards/{card}/assignee", s.setAssignee)
	mux.HandleFunc("POST /api/v1/cards/{card}/labels/{label}/toggle", s.toggleLabel)
	mux.HandleFunc("POST /api/v1/cards/{card}/archive", s.archiveCard)
	mux.HandleFunc("POST /api/v1/cards/{card}/restore", s.restoreCard)

	mux.HandleFunc("GET /api/v1/cards/{card}/comments", s.listComments)
	mux.HandleFunc("POST /api/v1/cards/{card}/comments", s.addComment)
	mux.HandleFunc("DELETE /api/v1/comments/{comment}", s.deleteComment)

	return s.jsonMisses(mux)
}

// jsonMisses answers a path or a method the mux has nothing for in JSON.
//
// The mux writes those two itself, as one line of text, which would make the
// unknown endpoint the only response in here a client cannot parse. Letting it
// decide which of the two the request is keeps the Allow header it puts on a
// 405, and the body is thrown away and replaced.
func (s *Server) jsonMisses(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		sw := &statusOnly{ResponseWriter: w}
		mux.ServeHTTP(sw, r)
		switch {
		case sw.status == http.StatusMethodNotAllowed:
			s.error(w, r, http.StatusMethodNotAllowed, "the endpoint does not answer this method")
		case sw.status >= 300 && sw.status < 400:
			// A path the mux wants to clean before it decides, which is what
			// /api/v1//cards is. It has already put the Location on the real
			// header, so passing the status through is the whole of it. The
			// missing endpoint is then answered by the second request, in JSON.
			//
			// The other redirect, /api/v1 to /api/v1/, does not come through
			// here: for that one the mux returns a pattern, so the branch above
			// hands it the writer and it serves its own body.
			w.WriteHeader(sw.status)
		default:
			s.error(w, r, http.StatusNotFound, "no such endpoint; GET /api/v1/openapi.json lists them")
		}
	})
}

// statusOnly records the status and drops the body, so the response the mux
// wrote can be replaced rather than appended to.
type statusOnly struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status without writing it, so a handler can be run
// to find out what it would answer.
func (s *statusOnly) WriteHeader(status int) { s.status = status }

// Write throws the body away and reports it written.
func (s *statusOnly) Write(p []byte) (int, error) { return len(p), nil }

// --- reading a request --------------------------------------------------------

// decode reads a JSON object into v.
//
// Unknown fields are refused. A caller here is a program, and "title" spelled
// "titel" silently creating a card with no title is the kind of bug that is
// found by the person reading the board a week later; a 400 that names the
// field is found by the person writing the script.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			s.error(w, r, http.StatusUnsupportedMediaType, "send application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			s.error(w, r, http.StatusBadRequest, "a JSON body is required")
			return false
		}
		s.error(w, r, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// board resolves the {board} path segment, which is a slug.
func (s *Server) board(w http.ResponseWriter, r *http.Request) (*model.Board, bool) {
	b, err := s.svc.Board(r.Context(), r.PathValue("board"))
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return b, true
}

// assignee resolves the one value that is not literal: "@me" is whoever is
// making the request, which is a thing only the server knows.
func (s *Server) assignee(w http.ResponseWriter, r *http.Request, want string) (string, bool) {
	if want != "@me" {
		return want, true
	}
	u := identity.FromContext(r.Context())
	// Person, not Anonymous: a service token is a caller with no address, and
	// "@me" asks for an address.
	if !u.Person() {
		s.error(w, r, http.StatusForbidden, "not signed in, so @me names nobody")
		return "", false
	}
	return u.Email, true
}

// --- writing a response -------------------------------------------------------

// write sends one JSON response. The body is marshalled before anything is
// written, so a value that cannot be encoded is a 500 rather than half a
// document with a 200 already on it.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	// Marshalled before anything is written, so a type that cannot be encoded
	// is a 500 rather than a 200 with half an object in it.
	buf, err := json.Marshal(body)
	if err != nil {
		s.log.Error("encode response", "method", r.Method, "path", r.URL.Path, "err", err)
		s.error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// error sends a JSON error with no field attached.
func (s *Server) error(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.fieldError(w, r, status, message, "")
}

// fieldError sends a JSON error naming the field that was wrong, so a client
// can put the message beside the input rather than at the top of a form.
func (s *Server) fieldError(w http.ResponseWriter, r *http.Request, status int, message, field string) {
	buf, err := json.Marshal(errorBody{Error: message, Field: field})
	if err != nil {
		// errorBody is two strings; this cannot happen, and if it does the
		// caller still deserves the status code.
		s.log.Error("encode error body", "err", err)
		buf = []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// fail maps a service or store error to a status code. It is the same mapping
// the pages use, in the body shape this package answers with.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message, field := statusFor(err)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	s.fieldError(w, r, status, message, field)
}

// statusFor is the mapping on its own, so a bulk result can report a per-card
// failure with the same words a single-card request would have answered.
func statusFor(err error) (status int, message, field string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		return http.StatusBadRequest, ve.Message, ve.Field
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not found", ""
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "already exists", ""
	case errors.Is(err, service.ErrWIPLimit):
		return http.StatusConflict, service.ErrWIPLimit.Error(), ""
	case errors.Is(err, service.ErrNotAuthor):
		return http.StatusForbidden, service.ErrNotAuthor.Error(), ""
	case errors.Is(err, store.ErrInvalid):
		return http.StatusBadRequest, "invalid request", ""
	}
	return http.StatusInternalServerError, "internal error", ""
}

// --- meta ---------------------------------------------------------------------

// index is what a person who types the prefix into a browser gets: the two
// links that lead everywhere else.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, http.StatusOK, indexBody{
		Version: "v1",
		Spec:    "/api/v1/openapi.json",
		Boards:  "/api/v1/boards",
	})
}
