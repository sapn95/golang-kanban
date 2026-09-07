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

	// The probe reports not ready without repeating the store's error. For
	// Postgres that error names the host, the port and the user, and a probe
	// path is the first thing anyone carves an auth bypass for.
	down := newEnv(t, nil, func(context.Context) error { return errors.New("db down at 10.42.0.7:5432") })
	ready := down.do(http.MethodGet, "/readyz", nil)
	want(t, ready, http.StatusServiceUnavailable, "not ready")
	if strings.Contains(ready.Body.String(), "10.42.0.7") {
		t.Errorf("the readiness probe leaked the database endpoint: %q", ready.Body.String())
	}
	// It still has to reach the operator, though.
	if !strings.Contains(down.log.String(), "db down at 10.42.0.7:5432") {
		t.Error("the store error was dropped instead of logged")
	}
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

func TestBulkEndpoint(t *testing.T) {
	post := func(t *testing.T, e *env, form url.Values, user *identity.User) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/b/demo/cards/bulk", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if user != nil {
			req = req.WithContext(identity.NewContext(req.Context(), *user))
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("delete removes the selected cards and asks for a refresh", func(t *testing.T) {
		e := seeded(t)
		rec := post(t, e, url.Values{"action": {"delete"}, "ids": {string(e.card.ID)}}, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("HX-Refresh") != "true" {
			t.Error("the client was not told to refresh")
		}
		if _, err := e.svc.Card(context.Background(), e.card.ID); err == nil {
			t.Error("the card survived a bulk delete")
		}
	})

	t.Run("assign to me needs a signed-in user", func(t *testing.T) {
		e := seeded(t)
		form := url.Values{"action": {"assign"}, "target": {"@me"}, "ids": {string(e.card.ID)}}

		// Anonymous: the sentinel must not be stored as though it were an
		// address, and must not silently assign to nobody either.
		if rec := post(t, e, form, nil); rec.Code != http.StatusForbidden {
			t.Errorf("anonymous status = %d, want 403", rec.Code)
		}
		if c, _ := e.svc.Card(context.Background(), e.card.ID); c.Assignee != "" {
			t.Errorf("assignee = %q after an anonymous assign-to-me", c.Assignee)
		}

		u := identity.User{Email: "someone@example.com"}
		if rec := post(t, e, form, &u); rec.Code != http.StatusNoContent {
			t.Fatalf("signed-in status = %d", rec.Code)
		}
		c, _ := e.svc.Card(context.Background(), e.card.ID)
		if c.Assignee != "someone@example.com" {
			t.Errorf("assignee = %q, want the signed-in address", c.Assignee)
		}
	})

	t.Run("an empty selection is a bad request", func(t *testing.T) {
		e := seeded(t)
		if rec := post(t, e, url.Values{"action": {"delete"}}, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("an unknown action is a bad request", func(t *testing.T) {
		e := seeded(t)
		rec := post(t, e, url.Values{"action": {"burn"}, "ids": {string(e.card.ID)}}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if _, err := e.svc.Card(context.Background(), e.card.ID); err != nil {
			t.Error("an unknown action still touched the card")
		}
	})

	t.Run("a card id from another board is not deleted", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		other, err := e.svc.CreateBoard(ctx, "Other", "other", nil)
		if err != nil {
			t.Fatal(err)
		}
		victim, err := e.svc.CreateCard(ctx, other.ID, other.Columns[0].ID, service.CardInput{Title: "theirs"})
		if err != nil {
			t.Fatal(err)
		}
		// Posted to /b/demo, so the handler must not reach across boards even
		// though the id is real.
		rec := post(t, e, url.Values{"action": {"delete"}, "ids": {string(victim.ID)}}, nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
		if _, err := e.svc.Card(ctx, victim.ID); err != nil {
			t.Error("a card on another board was deleted through this board's endpoint")
		}
	})
}

func TestEditFormMovesTheCard(t *testing.T) {
	post := func(t *testing.T, e *env, column model.ID) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"title": {"moved"}, "column": {string(column)}}
		req := httptest.NewRequest(http.MethodPost, "/cards/"+string(e.card.ID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("a different column moves it and redraws", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		b, err := e.svc.Board(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		target := b.Columns[1].ID

		rec := post(t, e, target)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("HX-Refresh") != "true" {
			t.Error("a move did not ask the page to redraw, so the card would stay drawn in the old column")
		}
		c, err := e.svc.Card(ctx, e.card.ID)
		if err != nil || c.ColumnID != target {
			t.Fatalf("column = %s (err %v), want %s", c.ColumnID, err, target)
		}
		if c.Title != "moved" {
			t.Errorf("title = %q, want the field change to have been saved too", c.Title)
		}
	})

	t.Run("the same column still swaps the card in place", func(t *testing.T) {
		e := seeded(t)
		b, _ := e.svc.Board(context.Background(), "demo")
		rec := post(t, e, b.Columns[0].ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want the card fragment", rec.Code)
		}
		if rec.Header().Get("HX-Refresh") != "" {
			t.Error("an unchanged column asked for a full redraw")
		}
	})

	t.Run("a refused move leaves the card untouched", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		b, _ := e.svc.Board(ctx, "demo")
		// A WIP limit of zero cards free: the move must be refused, and the
		// title must not have been written either.
		full := b.Columns[1]
		full.WIPLimit = 1
		if err := e.svc.UpdateColumn(ctx, full.ID, full.Name, 1); err != nil {
			t.Fatalf("setting a WIP limit: %v", err)
		}
		if _, err := e.svc.CreateCard(ctx, b.ID, full.ID, service.CardInput{Title: "occupies the slot"}); err != nil {
			t.Fatal(err)
		}

		before, _ := e.svc.Card(ctx, e.card.ID)
		rec := post(t, e, full.ID)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 for a full column", rec.Code)
		}
		after, _ := e.svc.Card(ctx, e.card.ID)
		if after.ColumnID != before.ColumnID {
			t.Error("the card moved even though the move was refused")
		}
		if after.Title != before.Title {
			t.Errorf("title = %q, want %q: the fields were written despite the refused move", after.Title, before.Title)
		}
	})
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	// Every one of these was demonstrated against a running instance before
	// the middleware existed: a form on another origin deleted cards.
	writes := []struct{ method, path, body, ctype string }{
		{http.MethodPost, "/b/demo/cards", "title=x&column=", "application/x-www-form-urlencoded"},
		{http.MethodPost, "/b/demo/cards/bulk", "action=delete&ids=x", "application/x-www-form-urlencoded"},
		{http.MethodPost, "/cards/x/delete", "", "application/x-www-form-urlencoded"},
		// The JSON route is not protected by being JSON: text/plain is a CORS
		// simple request and reaches the handler without a preflight.
		{http.MethodPost, "/b/demo/columns/c/order", `{"order":["x"]}`, "text/plain;charset=UTF-8"},
	}

	for _, w := range writes {
		t.Run(w.path, func(t *testing.T) {
			e := seeded(t)

			// Cross-site, the way a browser labels a request from another page.
			req := httptest.NewRequest(w.method, w.path, strings.NewReader(w.body))
			req.Header.Set("Content-Type", w.ctype)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			req.Header.Set("Origin", "https://evil.example")
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-site %s %s = %d, want 403", w.method, w.path, rec.Code)
			}

			// No Fetch Metadata at all: fall back to Origin.
			req = httptest.NewRequest(w.method, w.path, strings.NewReader(w.body))
			req.Header.Set("Content-Type", w.ctype)
			req.Header.Set("Origin", "https://evil.example")
			rec = httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("foreign Origin %s %s = %d, want 403", w.method, w.path, rec.Code)
			}

			// Same-origin still works: the middleware must not break the app.
			req = httptest.NewRequest(w.method, w.path, strings.NewReader(w.body))
			req.Header.Set("Content-Type", w.ctype)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rec = httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code == http.StatusForbidden {
				t.Errorf("same-origin %s %s was refused", w.method, w.path)
			}
		})
	}
}

func TestReadsAreNotRefusedCrossSite(t *testing.T) {
	// A GET changes nothing, and refusing them would break ordinary links.
	e := seeded(t)
	req := httptest.NewRequest(http.MethodGet, "/b/demo", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("cross-site GET = %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := seeded(t)
	rr := e.do(http.MethodGet, "/b/demo", nil)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		// The page carries the viewer's address; a shared cache must not keep it.
		"Cache-Control": "no-store",
	} {
		if got := rr.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	csp := rr.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}

	// Assets set their own long cache and must not be forced to no-store.
	if got := e.do(http.MethodGet, "/assets/app.js", nil).Header().Get("Cache-Control"); got == "no-store" {
		t.Error("assets were made uncacheable")
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	e := seeded(t)
	// Larger than maxBody. Before the limit this went to ParseMultipartForm,
	// which writes anything over 32MB to a file on the node.
	big := "name=" + strings.Repeat("a", 2<<20)
	req := httptest.NewRequest(http.MethodPost, "/boards", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
		t.Errorf("a %d-byte body was accepted (%d)", len(big), rec.Code)
	}
}

func TestArchiveThroughTheWeb(t *testing.T) {
	post := func(t *testing.T, e *env, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("archiving takes it off the board but keeps it", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()

		if rec := post(t, e, "/cards/"+string(e.card.ID)+"/archive"); rec.Code != http.StatusOK {
			t.Fatalf("archive = %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(e.do(http.MethodGet, "/b/demo", nil).Body.String(), "Archive") {
			t.Error("the board has no link to the archive")
		}
		if strings.Contains(e.do(http.MethodGet, "/b/demo", nil).Body.String(), string(e.card.ID)) {
			t.Error("an archived card is still drawn on the board")
		}
		// Not deleted: the archive shows it, and the card still resolves.
		if !strings.Contains(e.do(http.MethodGet, "/b/demo/archive", nil).Body.String(), string(e.card.ID)) {
			t.Error("the archived card is not in the archive")
		}
		if _, err := e.svc.Card(ctx, e.card.ID); err != nil {
			t.Errorf("the card was destroyed rather than archived: %v", err)
		}
	})

	t.Run("restoring sends the browser back to the board", func(t *testing.T) {
		e := seeded(t)
		if rec := post(t, e, "/cards/"+string(e.card.ID)+"/archive"); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
		rec := post(t, e, "/cards/"+string(e.card.ID)+"/restore?board=demo")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("restore = %d", rec.Code)
		}
		// The card comes back in a column the archive page is not showing, so
		// patching that page in place would leave it looking wrong.
		if got := rec.Header().Get("HX-Redirect"); got != "/b/demo" {
			t.Errorf("HX-Redirect = %q, want /b/demo", got)
		}
		if !strings.Contains(e.do(http.MethodGet, "/b/demo", nil).Body.String(), string(e.card.ID)) {
			t.Error("the restored card is not back on the board")
		}
	})

	t.Run("the empty archive says so", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo/archive", nil).Body.String()
		if !strings.Contains(body, "Nothing archived") {
			t.Error("an empty archive renders nothing useful")
		}
	})

	t.Run("bulk archive", func(t *testing.T) {
		e := seeded(t)
		form := url.Values{"action": {"archive"}, "ids": {string(e.card.ID)}}
		req := httptest.NewRequest(http.MethodPost, "/b/demo/cards/bulk", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("bulk archive = %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(e.do(http.MethodGet, "/b/demo/archive", nil).Body.String(), string(e.card.ID)) {
			t.Error("the card was not archived by the bulk action")
		}
	})
}
