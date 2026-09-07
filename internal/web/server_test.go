package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store"
	"kanban/internal/store/memory"
)

var today = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

type env struct {
	h     http.Handler
	svc   *service.Kanban
	board *model.Board
	card  *model.Card
	log   *bytes.Buffer
}

func newEnv(t *testing.T, st store.Store, ready func(context.Context) error) *env {
	t.Helper()
	if st == nil {
		st = memory.New()
	}
	svc := service.New(st, service.WithClock(func() time.Time { return today }))
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := New(svc, ready, logger, WithClock(func() time.Time { return today }))
	return &env{h: h, svc: svc, log: logBuf}
}

func seeded(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, nil, nil)
	ctx := context.Background()
	b, err := e.svc.CreateBoard(ctx, "Demo Board", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateLabel(ctx, b.ID, "bug", "#f00"); err != nil {
		t.Fatal(err)
	}
	b, _ = e.svc.Board(ctx, "demo")
	c, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{
		Title: "First card", Description: "desc", DueDate: "2026-09-01", Labels: []model.ID{b.Labels[0].ID},
		Subtasks: []model.Subtask{{Title: "sub one", Done: true}, {Title: "sub two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.board, e.card = b, c
	return e
}

func (e *env) do(method, path string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if body != nil && method == http.MethodPost && !strings.HasPrefix(path, "/b/") || strings.HasSuffix(path, "/cards") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func form(kv ...string) io.Reader {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return strings.NewReader(v.Encode())
}

func want(t *testing.T, rr *httptest.ResponseRecorder, status int, contains ...string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, status, rr.Body.String())
	}
	for _, s := range contains {
		if !strings.Contains(rr.Body.String(), s) {
			t.Fatalf("body missing %q:\n%s", s, rr.Body.String())
		}
	}
}

func TestIndex(t *testing.T) {
	e := newEnv(t, nil, nil)
	want(t, e.do(http.MethodGet, "/", nil), http.StatusOK, "No boards yet", `action="/boards"`)

	rr := e.do(http.MethodPost, "/boards", form("name", "Team One"), "Content-Type", "application/x-www-form-urlencoded")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/team-one" {
		t.Fatalf("create board: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	rr = e.do(http.MethodGet, "/", nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/team-one" {
		t.Fatalf("single board redirect: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	want(t, e.do(http.MethodPost, "/boards", form("name", "Team One"), "Content-Type", "application/x-www-form-urlencoded"),
		http.StatusBadRequest, "already exists")
	want(t, e.do(http.MethodPost, "/boards", form("name", "  "), "Content-Type", "application/x-www-form-urlencoded"),
		http.StatusBadRequest, "name: must not be empty")

	e.do(http.MethodPost, "/boards", form("name", "Team Two"), "Content-Type", "application/x-www-form-urlencoded")
	want(t, e.do(http.MethodGet, "/", nil), http.StatusOK, "/b/team-one", "/b/team-two", "3 columns")

	want(t, e.do(http.MethodGet, "/nope", nil), http.StatusNotFound)
	if rr := e.do(http.MethodDelete, "/boards", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method not allowed = %d", rr.Code)
	}
}

func TestBoardPage(t *testing.T) {
	e := seeded(t)
	rr := e.do(http.MethodGet, "/b/demo", nil)
	want(t, rr, http.StatusOK, "Demo Board", "To Do", "In Progress", "Done", "First card", "sub one", "sub two",
		`data-board="demo"`, "1 Sep 2026", "bg-red-100", ">bug<", `id="card-`+string(e.card.ID)+`"`,
		`hx-post="/b/demo/cards"`, `/assets/vendor/htmx/htmx.min.js`)
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	want(t, e.do(http.MethodGet, "/b/missing", nil), http.StatusNotFound)
}

func TestCreateCard(t *testing.T) {
	e := seeded(t)
	col := e.board.Columns[1].ID
	body := form("column", string(col), "title", "New one", "description", "d", "due_date", "2026-12-24",
		"labels", string(e.board.Labels[0].ID), "subtasks", "0|a\n1|b")
	rr := e.do(http.MethodPost, "/b/demo/cards", body, "HX-Request", "true")
	want(t, rr, http.StatusOK, "New one", ">a<", ">b<", "24 Dec 2026", ">bug<")
	if rr.Header().Get("HX-Retarget") != "#cards-"+string(col) || rr.Header().Get("HX-Reswap") != "beforeend" {
		t.Fatalf("htmx headers = %v", rr.Header())
	}
	cards, _ := e.svc.Cards(context.Background(), e.board.ID)
	if len(cards) != 2 || cards[1].ColumnID != col || len(cards[1].Subtasks) != 2 || !cards[1].Subtasks[1].Done {
		t.Fatalf("cards = %+v", cards)
	}

	// Non-HTMX form post redirects to the board; missing column means the first.
	rr = e.do(http.MethodPost, "/b/demo/cards", form("title", "Plain"))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/demo" {
		t.Fatalf("plain post: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	cards, _ = e.svc.Cards(context.Background(), e.board.ID)
	if len(cards) != 3 || cards[1].Title != "Plain" || cards[1].ColumnID != e.board.Columns[0].ID {
		t.Fatalf("cards = %+v", cards)
	}

	want(t, e.do(http.MethodPost, "/b/demo/cards", form("title", " "), "HX-Request", "true"), http.StatusBadRequest, "title: must not be empty")
	want(t, e.do(http.MethodPost, "/b/demo/cards", form("title", "x", "column", "nope")), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/nope/cards", form("title", "x")), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/cards", form("title", "x", "due_date", "soon")), http.StatusBadRequest, "due_date")
	want(t, e.do(http.MethodPost, "/b/demo/cards", strings.NewReader("%zz"), "Content-Type", "application/x-www-form-urlencoded"), http.StatusBadRequest, "bad form")

	if err := e.svc.UpdateColumn(context.Background(), col, "In Progress", 1); err != nil {
		t.Fatal(err)
	}
	want(t, e.do(http.MethodPost, "/b/demo/cards", form("title", "over", "column", string(col))), http.StatusConflict, "WIP limit")
}

func TestReorder(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	todo, doing := e.board.Columns[0].ID, e.board.Columns[1].ID
	second, _ := e.svc.CreateCard(ctx, e.board.ID, todo, service.CardInput{Title: "Second"})

	body := `{"order":["` + string(second.ID) + `","` + string(e.card.ID) + `"]}`
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(doing)+"/order", strings.NewReader(body)), http.StatusOK, "OK")
	cards, _ := e.svc.Cards(ctx, e.board.ID)
	if len(cards) != 2 || cards[0].ID != second.ID || cards[0].ColumnID != doing || cards[1].ColumnID != doing {
		t.Fatalf("cards = %+v", cards)
	}

	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(doing)+"/order", strings.NewReader("{")), http.StatusBadRequest, "invalid JSON")
	want(t, e.do(http.MethodPost, "/b/nope/columns/x/order", strings.NewReader(`{"order":[]}`)), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/columns/nope/order", strings.NewReader(`{"order":[]}`)), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", strings.NewReader(`{"order":["nope"]}`)), http.StatusNotFound)
	dup := `{"order":["` + string(second.ID) + `","` + string(second.ID) + `"]}`
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", strings.NewReader(dup)), http.StatusBadRequest, "invalid request")

	if err := e.svc.UpdateColumn(ctx, todo, "To Do", 1); err != nil {
		t.Fatal(err)
	}
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", strings.NewReader(body)), http.StatusConflict, "WIP limit")
}

func TestCardFragments(t *testing.T) {
	e := seeded(t)
	id := string(e.card.ID)
	want(t, e.do(http.MethodGet, "/cards/"+id, nil), http.StatusOK, "First card", `hx-get="/cards/`+id+`/edit"`)
	want(t, e.do(http.MethodGet, "/cards/"+id+"/edit", nil), http.StatusOK, `value="First card"`, `value="2026-09-01"`,
		`name="labels"`, "checked", `value="sub two"`, `hx-post="/cards/`+id+`"`)
	want(t, e.do(http.MethodGet, "/cards/nope", nil), http.StatusNotFound)
	want(t, e.do(http.MethodGet, "/cards/nope/edit", nil), http.StatusNotFound)

	rr := e.do(http.MethodPost, "/cards/"+id, form("title", "Edited", "subtasks", "1|done"), "HX-Request", "true", "Content-Type", "application/x-www-form-urlencoded")
	want(t, rr, http.StatusOK, "Edited", ">done<", "bi-check-square-fill")
	if strings.Contains(rr.Body.String(), "1 Sep 2026") {
		t.Fatal("due date should be cleared")
	}
	rr = e.do(http.MethodPost, "/cards/"+id, form("title", "Plain edit"), "Content-Type", "application/x-www-form-urlencoded")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/demo" {
		t.Fatalf("plain update: %d", rr.Code)
	}
	want(t, e.do(http.MethodPost, "/cards/"+id, form("title", ""), "Content-Type", "application/x-www-form-urlencoded"), http.StatusBadRequest, "title")
	want(t, e.do(http.MethodPost, "/cards/nope", form("title", "x"), "Content-Type", "application/x-www-form-urlencoded"), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/cards/"+id, strings.NewReader("%zz"), "Content-Type", "application/x-www-form-urlencoded"), http.StatusBadRequest, "bad form")

	want(t, e.do(http.MethodPost, "/cards/"+id+"/delete", nil), http.StatusOK)
	want(t, e.do(http.MethodPost, "/cards/"+id+"/delete", nil), http.StatusNotFound)
	if !strings.Contains(e.log.String(), "path=/cards/"+id+"/delete status=404") {
		t.Fatalf("request log missing:\n%s", e.log.String())
	}
}

func TestStaticAndHealth(t *testing.T) {
	e := newEnv(t, nil, nil)
	rr := e.do(http.MethodGet, "/assets/vendor/htmx/htmx.min.js", nil)
	want(t, rr, http.StatusOK, "htmx")
	if rr.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("cache header = %q", rr.Header().Get("Cache-Control"))
	}
	want(t, e.do(http.MethodGet, "/assets/app.js", nil), http.StatusOK, "postOrder")
	want(t, e.do(http.MethodGet, "/assets/app.css", nil), http.StatusOK, ".modal")
	want(t, e.do(http.MethodGet, "/assets/vendor/", nil), http.StatusNotFound)
	want(t, e.do(http.MethodGet, "/assets/", nil), http.StatusNotFound)
	want(t, e.do(http.MethodGet, "/assets/nope.js", nil), http.StatusNotFound)
	want(t, e.do(http.MethodGet, "/healthz", nil), http.StatusOK, "ok")
	want(t, e.do(http.MethodGet, "/readyz", nil), http.StatusOK, "ready")
	if rr := e.do(http.MethodGet, "/favicon.ico", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("favicon = %d", rr.Code)
	}

	down := newEnv(t, nil, func(context.Context) error { return errors.New("db down") })
	want(t, down.do(http.MethodGet, "/readyz", nil), http.StatusServiceUnavailable, "db down")
}

// broken fails or panics on ListBoards to exercise the 500 paths.
type broken struct {
	store.Store
	panic bool
}

func (b broken) ListBoards(context.Context) ([]model.Board, error) {
	if b.panic {
		panic("kaboom")
	}
	return nil, errors.New("disk on fire")
}

func TestServerErrors(t *testing.T) {
	e := newEnv(t, broken{Store: memory.New()}, nil)
	want(t, e.do(http.MethodGet, "/", nil), http.StatusInternalServerError, "internal error")
	if !strings.Contains(e.log.String(), "disk on fire") {
		t.Fatalf("error not logged: %s", e.log.String())
	}
	want(t, e.do(http.MethodPost, "/boards", form("name", ""), "Content-Type", "application/x-www-form-urlencoded"), http.StatusInternalServerError)

	p := newEnv(t, broken{Store: memory.New(), panic: true}, nil)
	want(t, p.do(http.MethodGet, "/", nil), http.StatusInternalServerError, "internal error")
	if !strings.Contains(p.log.String(), "kaboom") {
		t.Fatalf("panic not logged: %s", p.log.String())
	}
}

func TestNilLogger(t *testing.T) {
	h := New(service.New(memory.New()), nil, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
}

func TestConcurrentRequests(t *testing.T) {
	e := seeded(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rr := e.do(http.MethodGet, "/b/demo", nil); rr.Code != http.StatusOK {
				t.Errorf("status %d", rr.Code)
			}
		}()
	}
	wg.Wait()
}

func TestPagesShowTheSignedInUser(t *testing.T) {
	e := seeded(t)

	tests := []struct {
		name string
		path string
	}{
		{"board page", "/b/demo"},
		{"boards page", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req = req.WithContext(identity.NewContext(req.Context(),
				identity.User{Email: "someone@example.com"}))
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)

			// The boards page redirects when there is exactly one board, so
			// follow that rather than asserting on a 303 body.
			if rec.Code == http.StatusSeeOther {
				req = httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
				req = req.WithContext(identity.NewContext(req.Context(),
					identity.User{Email: "someone@example.com"}))
				rec = httptest.NewRecorder()
				e.h.ServeHTTP(rec, req)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "someone@example.com") {
				t.Error("the page does not name the signed-in user")
			}
			if !strings.Contains(body, ">someone<") && !strings.Contains(body, "someone\n") {
				t.Error("the page does not show the display name")
			}
		})
	}
}

func TestPagesSayNothingWhenNobodyIsSignedIn(t *testing.T) {
	e := seeded(t)
	req := httptest.NewRequest(http.MethodGet, "/b/demo", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	// "anonymous" is what Display falls back to; it must not be rendered as
	// though it were a signed-in user.
	if strings.Contains(rec.Body.String(), "anonymous") {
		t.Error("the page shows an avatar for a request with no user")
	}
}

func TestAssigneeThroughTheForm(t *testing.T) {
	e := seeded(t)

	post := func(t *testing.T, assignee string) string {
		t.Helper()
		form := url.Values{"title": {"assigned"}, "assignee": {assignee}}
		req := httptest.NewRequest(http.MethodPost, "/cards/"+string(e.card.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := post(t, "someone@example.com")
	if !strings.Contains(body, "someone@example.com") {
		t.Error("the rendered card does not show the assignee")
	}

	// Unassigning has to actually clear it, not be read as "unchanged".
	body = post(t, "")
	if strings.Contains(body, "someone@example.com") {
		t.Error("the assignee survived being cleared")
	}

	// Whitespace is not an assignee.
	body = post(t, "   ")
	if strings.Contains(body, "avatar") || strings.Contains(body, "title=\"   \"") {
		t.Error("whitespace was stored as an assignee")
	}
}

func TestAssigneeIsMarkedWhenItIsTheViewer(t *testing.T) {
	e := seeded(t)
	form := url.Values{"title": {"mine"}, "assignee": {"someone@example.com"}}
	req := httptest.NewRequest(http.MethodPost, "/cards/"+string(e.card.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(identity.NewContext(req.Context(), identity.User{Email: "someone@example.com"}))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	// The viewer's own cards get the gradient bubble; everyone else's is grey.
	if !strings.Contains(rec.Body.String(), "from-blue-600 to-purple-600") {
		t.Error("a card assigned to the viewer is not marked as theirs")
	}
}
