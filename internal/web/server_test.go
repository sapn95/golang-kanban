package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kanban/assets"
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

func newEnv(t *testing.T, st store.Store, ready func(context.Context) error, opts ...Option) *env {
	t.Helper()
	if st == nil {
		st = memory.New()
	}
	svc := service.New(st, service.WithClock(func() time.Time { return today }))
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := New(svc, ready, logger, append([]Option{WithClock(func() time.Time { return today })}, opts...)...)
	return &env{h: h, svc: svc, log: logBuf}
}

func seeded(t *testing.T, opts ...Option) *env {
	t.Helper()
	e := newEnv(t, nil, nil, opts...)
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
	// Every write on these pages is a form now that the card order is one too,
	// so a POST with a body gets the header a browser would send. A subtest
	// that wants a different one passes it in headers, which is applied after.
	if body != nil && method == http.MethodPost {
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
		`data-board="demo"`, "1 Sep 2026", "bg-rose-100", ">bug<", `id="card-`+string(e.card.ID)+`"`,
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

	if err := e.svc.UpdateColumn(context.Background(), col, "In Progress", 1, false); err != nil {
		t.Fatal(err)
	}
	want(t, e.do(http.MethodPost, "/b/demo/cards", form("title", "over", "column", string(col))), http.StatusConflict, "WIP limit")
}

func TestReorder(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	todo, doing := e.board.Columns[0].ID, e.board.Columns[1].ID
	second, _ := e.svc.CreateCard(ctx, e.board.ID, todo, service.CardInput{Title: "Second"})

	order := func(ids ...model.ID) io.Reader {
		v := url.Values{}
		for _, id := range ids {
			v.Add("order", string(id))
		}
		return strings.NewReader(v.Encode())
	}
	// The answer is the board's column headers, marked for htmx to put back
	// where they belong: the column the cards left is redrawn as well as the
	// one they arrived in.
	res := e.do(http.MethodPost, "/b/demo/columns/"+string(doing)+"/order", order(second.ID, e.card.ID))
	want(t, res, http.StatusOK, `id="colhead-`+string(doing)+`" hx-swap-oob="true"`)
	if body := res.Body.String(); !strings.Contains(body, `id="colhead-`+string(todo)+`"`) {
		t.Error("the column the cards left was not redrawn")
	}
	cards, _ := e.svc.Cards(ctx, e.board.ID)
	if len(cards) != 2 || cards[0].ID != second.ID || cards[0].ColumnID != doing || cards[1].ColumnID != doing {
		t.Fatalf("cards = %+v", cards)
	}

	want(t, e.do(http.MethodPost, "/b/nope/columns/x/order", order()), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/columns/nope/order", order()), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", order("nope")), http.StatusNotFound)
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", order(second.ID, second.ID)), http.StatusBadRequest, "invalid request")
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", strings.NewReader("%zz"), "Content-Type", "application/x-www-form-urlencoded"), http.StatusBadRequest, "bad form")

	if err := e.svc.UpdateColumn(ctx, todo, "To Do", 1, false); err != nil {
		t.Fatal(err)
	}
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(todo)+"/order", order(second.ID, e.card.ID)), http.StatusConflict, "WIP limit")
}

// Dragging a selection posts one order to the destination column, and the cards
// in it can come from anywhere on the board. The browser does the gathering, so
// what this pins down is the contract it relies on.
func TestReorderTakesCardsFromSeveralColumnsAtOnce(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	doing, done := e.board.Columns[1].ID, e.board.Columns[2].ID
	inDoing, _ := e.svc.CreateCard(ctx, e.board.ID, doing, service.CardInput{Title: "In doing"})

	// e.card is in To Do, inDoing is in In Progress, and both are dropped into
	// Done in one request.
	v := url.Values{"order": {string(inDoing.ID), string(e.card.ID)}}
	want(t, e.do(http.MethodPost, "/b/demo/columns/"+string(done)+"/order", strings.NewReader(v.Encode())),
		http.StatusOK, `id="colhead-`+string(done)+`"`)

	cards, _ := e.svc.Cards(ctx, e.board.ID)
	for _, c := range cards {
		if c.ColumnID != done {
			t.Errorf("%q stayed in %s, so the rest of the selection was left behind", c.Title, c.ColumnID)
		}
	}
	if len(cards) != 2 || cards[0].ID != inDoing.ID {
		t.Fatalf("the dropped order was not kept: %+v", cards)
	}
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
	if rr.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
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
	if !strings.Contains(rec.Body.String(), "from-indigo-500 to-violet-500") {
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
		if err := e.svc.UpdateColumn(ctx, full.ID, full.Name, 1, false); err != nil {
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

func TestSearchTheArchive(t *testing.T) {
	setup := func(t *testing.T) *env {
		t.Helper()
		e := seeded(t)
		ctx := context.Background()
		b, err := e.svc.Board(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		for _, title := range []string{"Fix the login page", "Rotate the certificates"} {
			c, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{Title: title})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.svc.ArchiveCard(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
		}
		// e.card, "First card", stays on the board.
		return e
	}

	t.Run("the archive has a box of its own", func(t *testing.T) {
		e := setup(t)
		want(t, e.do(http.MethodGet, "/b/demo/archive", nil), http.StatusOK,
			`action="/b/demo/archive"`, `type="search" name="q"`)
	})

	t.Run("a word narrows the archive and is echoed back", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo/archive?q=login", nil).Body.String()
		if !strings.Contains(body, "Fix the login page") {
			t.Error("the matching archived card is not in the results")
		}
		if strings.Contains(body, "Rotate the certificates") {
			t.Error("a card that does not match was still drawn")
		}
		if !strings.Contains(body, `value="login"`) {
			t.Error("the query was not put back in the box, so a reload would lose it")
		}
		if !strings.Contains(body, "1 matching") {
			t.Error("the count still says how much is archived rather than how much matched")
		}
	})

	t.Run("a typo finds it anyway", func(t *testing.T) {
		e := setup(t)
		if !strings.Contains(e.do(http.MethodGet, "/b/demo/archive?q=logni", nil).Body.String(), "Fix the login page") {
			t.Error("a swapped pair of letters lost the card")
		}
	})

	t.Run("the search stays in the archive", func(t *testing.T) {
		e := setup(t)
		// "First card" is on the board and matches the word, so a search that
		// returned it would be answering a question this page did not ask.
		for _, path := range []string{"/b/demo/archive?q=card", "/b/demo/archive?q=card+is%3Aarchived"} {
			if strings.Contains(e.do(http.MethodGet, path, nil).Body.String(), "First card") {
				t.Errorf("%s returned a card that is still on the board", path)
			}
		}
	})

	t.Run("the board's syntax works here too", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo/archive?q=label%3Abug", nil).Body.String()
		if strings.Contains(body, "Fix the login page") {
			t.Error("label: did not filter the archive")
		}
	})

	t.Run("nothing matching says so, and not that the archive is empty", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo/archive?q=nothingmatchesthis", nil).Body.String()
		if !strings.Contains(body, "Nothing in the archive matches that") {
			t.Error("an empty result set rendered nothing useful")
		}
		if strings.Contains(body, "Nothing archived") {
			t.Error("a search with no hits claimed the archive is empty")
		}
	})

	t.Run("no query is the whole archive", func(t *testing.T) {
		e := setup(t)
		for _, path := range []string{"/b/demo/archive", "/b/demo/archive?q=", "/b/demo/archive?q=%20%20"} {
			body := e.do(http.MethodGet, path, nil).Body.String()
			for _, title := range []string{"Fix the login page", "Rotate the certificates"} {
				if !strings.Contains(body, title) {
					t.Errorf("%s did not show %q", path, title)
				}
			}
			if !strings.Contains(body, "2 archived") {
				t.Errorf("%s did not count the archive", path)
			}
		}
	})
}

// The settings page hides a row's Save until that row changes, and app.js finds
// the buttons by class. Losing the class would put all six of them back on the
// page and nothing would fail, so the markup is asserted here.
func TestEverySettingsRowSaveIsMarked(t *testing.T) {
	e := seeded(t)
	b, err := e.svc.Board(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	body := e.do(http.MethodGet, "/b/demo/settings", nil).Body.String()
	// One per column row and one per label row. The two "+ Add" buttons are not
	// rows: there is nothing to save until something has been typed anyway, and
	// they are the only way to add.
	if got, want := strings.Count(body, `class="row-save`), len(b.Columns)+len(b.Labels); got != want {
		t.Errorf("%d marked Save buttons, want %d (%d columns, %d labels)", got, want, len(b.Columns), len(b.Labels))
	}
}

// A search box with nothing but a placeholder is announced as an unnamed field
// once somebody starts typing, which is when the placeholder disappears.
func TestEverySearchBoxIsNamed(t *testing.T) {
	e := seeded(t)
	box := regexp.MustCompile(`(?s)<input[^>]*type="search"[^>]*>`)
	for _, path := range []string{"/b/demo", "/b/demo?q=login", "/b/demo/archive", "/b/demo/archive?q=login"} {
		body := e.do(http.MethodGet, path, nil).Body.String()
		found := box.FindAllString(body, -1)
		if len(found) == 0 {
			t.Errorf("%s: no search box", path)
		}
		for _, tag := range found {
			if !strings.Contains(tag, "aria-label=") {
				t.Errorf("%s: search box with no aria-label: %s", path, tag)
			}
		}
	}
}

func TestSearchOnTheBoardURL(t *testing.T) {
	setup := func(t *testing.T) *env {
		t.Helper()
		e := seeded(t)
		ctx := context.Background()
		b, err := e.svc.Board(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID,
			service.CardInput{Title: "Fix the login page", Description: "throws on submit"}); err != nil {
			t.Fatal(err)
		}
		gone, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{Title: "old login work"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.ArchiveCard(ctx, gone.ID); err != nil {
			t.Fatal(err)
		}
		return e
	}

	t.Run("a hit is shown and the query is echoed back", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo?q=login", nil).Body.String()
		if !strings.Contains(body, "Fix the login page") {
			t.Error("the matching card is not in the results")
		}
		if !strings.Contains(body, `value="login"`) {
			t.Error("the query was not put back in the box, so a reload would lose it")
		}
	})

	t.Run("the board's own cards are not all shown during a search", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo?q=nothingmatchesthis", nil).Body.String()
		if !strings.Contains(body, "Nothing matches that") {
			t.Error("an empty result set silently rendered the board instead of saying so")
		}
		if strings.Contains(body, string(e.card.ID)) {
			t.Error("a non-matching card was still drawn")
		}
	})

	t.Run("archived cards are only found with is:archived", func(t *testing.T) {
		e := setup(t)
		plain := e.do(http.MethodGet, "/b/demo?q=login", nil).Body.String()
		if strings.Contains(plain, "old login work") {
			t.Error("an archived card turned up in an ordinary search")
		}
		arch := e.do(http.MethodGet, "/b/demo?q=login+is%3Aarchived", nil).Body.String()
		if !strings.Contains(arch, "old login work") {
			t.Error("is:archived did not reach the archive")
		}
		if strings.Contains(arch, "Fix the login page") {
			t.Error("is:archived also returned cards that are still on the board")
		}
	})

	t.Run("an empty query is the plain board", func(t *testing.T) {
		e := setup(t)
		for _, path := range []string{"/b/demo", "/b/demo?q=", "/b/demo?q=%20%20"} {
			body := e.do(http.MethodGet, path, nil).Body.String()
			if strings.Contains(body, "Nothing matches that") {
				t.Errorf("%s rendered a search with no hits instead of the board", path)
			}
			if !strings.Contains(body, "Add New Card") {
				t.Errorf("%s did not render the board", path)
			}
		}
	})
}

// doAs makes a request carrying an identity, the way the middleware would have
// put one on the context.
func (e *env) doAs(user, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if user != "" {
		req = req.WithContext(identity.NewContext(req.Context(), identity.User{Email: user}))
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func TestCommentsThroughTheWeb(t *testing.T) {
	const her, him = "her@example.com", "him@example.com"

	setup := func(t *testing.T) (*env, string) {
		t.Helper()
		e := seeded(t)
		return e, "/cards/" + string(e.card.ID)
	}

	t.Run("an empty thread invites one and the card carries no badge", func(t *testing.T) {
		e, card := setup(t)
		want(t, e.doAs(her, http.MethodGet, card+"/edit", nil), http.StatusOK,
			"Nothing said yet", "Comments cannot be edited")
		if body := e.do(http.MethodGet, "/b/demo", nil).Body.String(); strings.Contains(body, "bi-chat-left-text") {
			t.Error("a card with no comments still shows the badge")
		}
	})

	t.Run("posting answers with the comment and the refreshed card face", func(t *testing.T) {
		e, card := setup(t)
		rr := e.doAs(her, http.MethodPost, card+"/comments", form("body", "looks fine to me"))
		// The card face rides along out of band, so the badge on the board
		// behind the modal is not left saying nothing has been said.
		want(t, rr, http.StatusOK, "looks fine to me", `hx-swap-oob="true"`, "bi-chat-left-text")

		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK, "bi-chat-left-text")
		want(t, e.doAs(her, http.MethodGet, card+"/edit", nil), http.StatusOK, "looks fine to me")
	})

	t.Run("only the author is offered the remove button", func(t *testing.T) {
		e, card := setup(t)
		e.doAs(her, http.MethodPost, card+"/comments", form("body", "hers"))

		mine := e.doAs(her, http.MethodGet, card+"/edit", nil).Body.String()
		if !strings.Contains(mine, "/delete") {
			t.Error("the author is not offered a way to remove their own comment")
		}
		theirs := e.doAs(him, http.MethodGet, card+"/edit", nil).Body.String()
		if strings.Contains(theirs, "/delete") {
			t.Error("someone else is offered a remove button they cannot use")
		}
	})

	t.Run("someone else cannot remove it", func(t *testing.T) {
		e, card := setup(t)
		e.doAs(her, http.MethodPost, card+"/comments", form("body", "hers"))
		id := commentID(t, e, e.card.ID)

		want(t, e.doAs(him, http.MethodPost, "/comments/"+id+"/delete", nil), http.StatusForbidden)
		if n := len(comments(t, e, e.card.ID)); n != 1 {
			t.Errorf("the comment count is %d after a refused delete, want 1", n)
		}
	})

	t.Run("the author can, and the badge goes with it", func(t *testing.T) {
		e, card := setup(t)
		e.doAs(her, http.MethodPost, card+"/comments", form("body", "hers"))
		id := commentID(t, e, e.card.ID)

		rr := e.doAs(her, http.MethodPost, "/comments/"+id+"/delete", nil)
		want(t, rr, http.StatusOK, `hx-swap-oob="true"`)
		// The response replaces the comment with whatever is left after the
		// out-of-band card is lifted out, so it must not still hold the text.
		if strings.Contains(rr.Body.String(), "hers") {
			t.Error("the removed comment came back in the response")
		}
		if strings.Contains(rr.Body.String(), "bi-chat-left-text") {
			t.Error("the refreshed card still shows a comment badge")
		}
		if n := len(comments(t, e, e.card.ID)); n != 0 {
			t.Errorf("%d comments left after the author removed the only one", n)
		}
	})

	t.Run("an empty comment is refused and an unknown card is a miss", func(t *testing.T) {
		e, card := setup(t)
		want(t, e.doAs(her, http.MethodPost, card+"/comments", form("body", "   ")), http.StatusBadRequest)
		want(t, e.doAs(her, http.MethodPost, "/cards/nope/comments", form("body", "hello")), http.StatusNotFound)
		want(t, e.doAs(her, http.MethodPost, "/comments/nope/delete", nil), http.StatusNotFound)
	})
}

func comments(t *testing.T, e *env, card model.ID) []model.Comment {
	t.Helper()
	list, err := e.svc.Comments(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func commentID(t *testing.T, e *env, card model.ID) string {
	t.Helper()
	list := comments(t, e, card)
	if len(list) == 0 {
		t.Fatal("no comments on the card")
	}
	return string(list[0].ID)
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		then time.Time
		want string
	}{
		{"seconds", now.Add(-20 * time.Second), "just now"},
		{"one minute", now.Add(-time.Minute), "1 minute ago"},
		{"minutes", now.Add(-40 * time.Minute), "40 minutes ago"},
		{"one hour", now.Add(-time.Hour), "1 hour ago"},
		{"hours", now.Add(-5 * time.Hour), "5 hours ago"},
		{"one day", now.Add(-25 * time.Hour), "1 day ago"},
		{"days", now.AddDate(0, 0, -3), "3 days ago"},
		// Past a month the elapsed time stops being the useful thing to say.
		{"long ago", now.AddDate(0, 0, -40), "27 Jul 2026"},
		// A clock that has drifted backwards must not read "-3 minutes ago".
		{"in the future", now.Add(time.Hour), "just now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ago(now, tt.then); got != tt.want {
				t.Errorf("ago = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoardPalette(t *testing.T) {
	label := func(color string) model.Label { return model.Label{Color: color} }
	cases := []struct {
		name  string
		in    []model.Label
		extra []string
	}{
		{"a board on the presets adds nothing", []model.Label{label("#ef4444"), label("#6b7280")}, nil},
		{"a label with no colour adds nothing", []model.Label{label("")}, nil},
		{"a colour off the presets is offered", []model.Label{label("#abcdef")}, []string{"#abcdef"}},
		{"the same colour twice is one swatch", []model.Label{label("#abcdef"), label("#abcdef")}, []string{"#abcdef"}},
		{"several are sorted, not in label order", []model.Label{label("#fedcba"), label("#abcdef")}, []string{"#abcdef", "#fedcba"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := boardPalette(tt.in)
			want := append(slices.Clone(labelPalette), tt.extra...)
			if !slices.Equal(got, want) {
				t.Errorf("boardPalette() = %v, want %v", got, want)
			}
		})
	}
}

// Every row offering the same colours is the point: a colour set through the
// API used to be offered to the label that carried it and to nothing else, so
// no second label could be given it and the rows were different lengths.
func TestEveryLabelRowOffersTheSameColours(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	b, err := e.svc.Board(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// #0ea5e9 is not one of the presets, which is what an API client is free to
	// do and what the form has to keep offering afterwards.
	if _, err := e.svc.CreateLabel(ctx, b.ID, "homelab", "#0ea5e9"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateLabel(ctx, b.ID, "kanban", "#8b5cf6"); err != nil {
		t.Fatal(err)
	}
	body := e.do(http.MethodGet, "/b/demo/settings", nil).Body.String()

	group := regexp.MustCompile(`(?s)aria-label="Colour".*?</div>`)
	groups := group.FindAllString(body, -1)
	// One per label row and one for the new-label form.
	if want := 4; len(groups) != want {
		t.Fatalf("%d swatch groups, want %d", len(groups), want)
	}
	for i, g := range groups {
		if got, want := strings.Count(g, `name="color"`), len(labelPalette)+2; got != want {
			t.Errorf("group %d offers %d colours, want %d (the presets, no colour, and the one off the list)", i, got, want)
		}
		if !strings.Contains(g, "#0ea5e9") {
			t.Errorf("group %d does not offer #0ea5e9, the colour another label carries", i)
		}
	}
}

func TestLabelsThroughTheWeb(t *testing.T) {
	// A same-origin form post, which is what the browser sends and what the
	// cross-site middleware is looking for.
	post := func(e *env, path string, body io.Reader) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, path, body, "Content-Type", "application/x-www-form-urlencoded",
			"Sec-Fetch-Site", "same-origin")
	}

	t.Run("the page lists the board's labels and how they are used", func(t *testing.T) {
		e := seeded(t)
		rr := e.do(http.MethodGet, "/b/demo/settings", nil)
		// The seeded board has one label, on the seeded card.
		want(t, rr, http.StatusOK, "bug", "on 1 card", "New label")
	})

	t.Run("a label can be made from the form", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/labels", form("name", "chore", "color", "#22c55e")), http.StatusSeeOther)

		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		var found *model.Label
		for i := range b.Labels {
			if b.Labels[i].Name == "chore" {
				found = &b.Labels[i]
			}
		}
		if found == nil {
			t.Fatal("the label was not created")
		}
		if found.Color != "#22c55e" {
			t.Errorf("colour = %q, want the one the form sent", found.Color)
		}
	})

	t.Run("a colour the template could not render is refused with a reason", func(t *testing.T) {
		e := seeded(t)
		rr := post(e, "/b/demo/labels", form("name", "bad", "color", "javascript:alert(1)"))
		// A bad colour used to reach the style attribute and render as
		// ZgotmplZ, which reads as a bug rather than as a rejection.
		want(t, rr, http.StatusBadRequest, "hex colour")
		if strings.Contains(rr.Body.String(), "ZgotmplZ") {
			t.Error("the page rendered ZgotmplZ instead of rejecting the colour")
		}
	})

	t.Run("a duplicate name says so instead of failing silently", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/labels", form("name", "bug", "color", "")), http.StatusConflict,
			"already has a label with that name")
	})

	t.Run("renaming and recolouring reach every card at once", func(t *testing.T) {
		e := seeded(t)
		id := string(e.board.Labels[0].ID)
		want(t, post(e, "/b/demo/labels/"+id, form("name", "defect", "color", "#ef4444")), http.StatusSeeOther)

		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK, "defect", "#ef4444")
	})

	t.Run("a label from another board cannot be reached through this one", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		other, err := e.svc.CreateBoard(ctx, "Other", "other", nil)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := e.svc.CreateLabel(ctx, other.ID, "theirs", "#3b82f6")
		if err != nil {
			t.Fatal(err)
		}
		want(t, post(e, "/b/demo/labels/"+string(theirs.ID), form("name", "mine", "color", "")), http.StatusNotFound)
		want(t, post(e, "/b/demo/labels/"+string(theirs.ID)+"/delete", nil), http.StatusNotFound)

		still, err := e.svc.Board(ctx, "other")
		if err != nil {
			t.Fatal(err)
		}
		if len(still.Labels) != 1 || still.Labels[0].Name != "theirs" {
			t.Errorf("the other board's labels are %v, want them untouched", still.Labels)
		}
	})

	t.Run("deleting takes it off the cards", func(t *testing.T) {
		e := seeded(t)
		id := string(e.board.Labels[0].ID)
		want(t, post(e, "/b/demo/labels/"+id+"/delete", nil), http.StatusSeeOther)

		card, err := e.svc.Card(context.Background(), e.card.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(card.Labels) != 0 {
			t.Errorf("the card still carries %v after the label was deleted", card.Labels)
		}
		want(t, e.do(http.MethodGet, "/b/demo/settings", nil), http.StatusOK, "0 labels")
	})
}

func TestDueState(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	now := day(5)
	tests := []struct {
		name string
		due  time.Time
		want string
	}{
		{"yesterday", day(4), "overdue"},
		{"today", day(5), "today"},
		{"tomorrow", day(6), "soon"},
		{"the last day that still counts as soon", day(8), "soon"},
		{"one day past that", day(9), "later"},
		{"next month", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), "later"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dueState(now, tt.due); got != tt.want {
				t.Errorf("dueState = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoardShowsWhatIsPressing(t *testing.T) {
	t.Run("the checklist shows its progress on the card face", func(t *testing.T) {
		e := seeded(t)
		// The seeded card has two subtasks, one of them done.
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, ">1/2<") {
			t.Error("the card face does not say how much of the checklist is done")
		}
		if !strings.Contains(body, "width: 50%") {
			t.Error("the progress bar is not drawn at half")
		}
		if strings.Contains(body, "ZgotmplZ") {
			t.Error("the width was refused by the template's CSS escaper")
		}
	})

	t.Run("a WIP limit is visible before a drop is refused", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		todo := e.board.Columns[0]

		// One card in the column already, so a limit of 1 is a full column
		// rather than an over-full one.
		if err := e.svc.UpdateColumn(ctx, todo.ID, todo.Name, 1, false); err != nil {
			t.Fatal(err)
		}
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, "1 / 1") {
			t.Error("the column header does not show the count against the limit")
		}
		if !strings.Contains(body, "At the limit.") {
			t.Error("a full column says nothing until a drop is refused")
		}
		if !strings.Contains(body, "bg-amber-500") {
			t.Error("the limit bar is not amber on a full column")
		}
		// One notice at a time. A drop answers with the header rendered again,
		// so the copy for a state the column is not in has no reason to be in
		// the page waiting to be unhidden.
		if strings.Contains(body, "over its limit") {
			t.Error("a full column shows the over-limit notice as well")
		}
	})

	t.Run("an over-full column says by how much", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		todo := e.board.Columns[0]
		if _, err := e.svc.CreateCard(ctx, e.board.ID, todo.ID, service.CardInput{Title: "second"}); err != nil {
			t.Fatal(err)
		}
		// Set the limit after the fact: a limit lowered under a column that is
		// already fuller than it is the case the board has to survive.
		if err := e.svc.UpdateColumn(ctx, todo.ID, todo.Name, 1, false); err != nil {
			t.Fatal(err)
		}
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, "2 / 1") {
			t.Error("the header does not show the column past its limit")
		}
		if !strings.Contains(body, "bg-rose-500") {
			t.Error("the limit bar is not red on an over-full column")
		}
		// Capped, so the bar does not draw outside its own track.
		if !strings.Contains(body, "width: 100%") {
			t.Errorf("the bar was not capped at 100%%")
		}
	})

	t.Run("a column without a limit draws no bar", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if strings.Contains(body, "data-limit-bar-for") {
			t.Error("a column with no WIP limit still draws a limit bar")
		}
	})
}

func TestDueDatesAreGradedOnTheCardFace(t *testing.T) {
	tests := []struct {
		name string
		due  string
		want string
	}{
		{"overdue is red", "2026-09-01", "bg-rose-100"},
		{"today is amber", "2026-09-05", "bg-amber-100"},
		{"within days is yellow", "2026-09-07", "bg-yellow-100"},
		{"further out is blue", "2026-11-01", "bg-sky-100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil, nil)
			ctx := context.Background()
			b, err := e.svc.CreateBoard(ctx, "Dates", "dates", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{
				Title: "dated", DueDate: tt.due,
			}); err != nil {
				t.Fatal(err)
			}
			want(t, e.do(http.MethodGet, "/b/dates", nil), http.StatusOK, tt.want)
		})
	}
}

func TestColumnsThroughTheWeb(t *testing.T) {
	post := func(e *env, path string, body io.Reader) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, path, body, "Content-Type", "application/x-www-form-urlencoded",
			"Sec-Fetch-Site", "same-origin")
	}
	names := func(t *testing.T, e *env) []string {
		t.Helper()
		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range b.Columns {
			out = append(out, c.Name)
		}
		return out
	}

	t.Run("the page lists the columns and what they hold", func(t *testing.T) {
		e := seeded(t)
		// The seeded board is To Do / In Progress / Done, with one card.
		want(t, e.do(http.MethodGet, "/b/demo/settings", nil), http.StatusOK,
			"3 columns", "1 card", "empty", "New column", "WIP limit")
	})

	t.Run("the old labels URL still lands somewhere", func(t *testing.T) {
		e := seeded(t)
		rr := e.do(http.MethodGet, "/b/demo/labels", nil)
		if rr.Code != http.StatusMovedPermanently {
			t.Fatalf("status = %d, want a permanent redirect", rr.Code)
		}
		if got := rr.Header().Get("Location"); got != "/b/demo/settings" {
			t.Errorf("Location = %q, want the settings page", got)
		}
	})

	t.Run("a column can be added with a WIP limit", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/columns", form("name", "Review", "wip_limit", "2")), http.StatusSeeOther)

		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		last := b.Columns[len(b.Columns)-1]
		if last.Name != "Review" || last.WIPLimit != 2 {
			t.Errorf("added column = %+v, want Review with a limit of 2", last)
		}
	})

	t.Run("an empty limit means no limit", func(t *testing.T) {
		e := seeded(t)
		todo := e.board.Columns[0]
		want(t, post(e, "/b/demo/columns/"+string(todo.ID), form("name", todo.Name, "wip_limit", "")), http.StatusSeeOther)

		b, _ := e.svc.Board(context.Background(), "demo")
		if b.Columns[0].WIPLimit != 0 {
			t.Errorf("limit = %d, want 0 for an empty field", b.Columns[0].WIPLimit)
		}
	})

	t.Run("a limit that is not a number says so on the form", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/columns", form("name", "Bad", "wip_limit", "lots")), http.StatusBadRequest,
			"whole number")
		if len(names(t, e)) != 3 {
			t.Error("the column was created despite the rejected limit")
		}
	})

	t.Run("a limit set here is what the board then shows", func(t *testing.T) {
		e := seeded(t)
		todo := e.board.Columns[0]
		want(t, post(e, "/b/demo/columns/"+string(todo.ID), form("name", "To Do", "wip_limit", "1")), http.StatusSeeOther)
		// The whole point of the editor: the limit was in the model from the
		// start and there was no way to set it.
		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK, "1 / 1", "At the limit.")
	})

	t.Run("columns move one place at a time and stop at the ends", func(t *testing.T) {
		e := seeded(t)
		second := e.board.Columns[1]
		want(t, post(e, "/b/demo/columns/"+string(second.ID)+"/move", form("direction", "up")), http.StatusSeeOther)
		if got := names(t, e); got[0] != "In Progress" || got[1] != "To Do" {
			t.Errorf("order = %v, want In Progress moved ahead of To Do", got)
		}
		// Already first. A repeated submit is a no-op, not an error page.
		want(t, post(e, "/b/demo/columns/"+string(second.ID)+"/move", form("direction", "up")), http.StatusSeeOther)
		if got := names(t, e); got[0] != "In Progress" {
			t.Errorf("order = %v, want it left alone at the end", got)
		}
	})

	t.Run("deleting takes the cards somewhere, or with it", func(t *testing.T) {
		e := seeded(t)
		todo, done := e.board.Columns[0], e.board.Columns[2]
		want(t, post(e, "/b/demo/columns/"+string(todo.ID)+"/delete",
			form("move_to", string(done.ID))), http.StatusSeeOther)

		card, err := e.svc.Card(context.Background(), e.card.ID)
		if err != nil {
			t.Fatalf("the card went with the column: %v", err)
		}
		if card.ColumnID != done.ID {
			t.Errorf("the card is in %s, want it moved to %s", card.ColumnID, done.ID)
		}
		if got := names(t, e); len(got) != 2 {
			t.Errorf("columns = %v, want the deleted one gone", got)
		}
	})

	t.Run("an empty destination deletes the cards too", func(t *testing.T) {
		e := seeded(t)
		todo := e.board.Columns[0]
		want(t, post(e, "/b/demo/columns/"+string(todo.ID)+"/delete", form("move_to", "")), http.StatusSeeOther)
		if _, err := e.svc.Card(context.Background(), e.card.ID); err == nil {
			t.Error("the card survived a delete with nowhere to move it")
		}
	})

	t.Run("the last column cannot go", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		b, err := e.svc.CreateBoard(ctx, "Single", "single", []string{"Only"})
		if err != nil {
			t.Fatal(err)
		}
		want(t, post(e, "/b/single/columns/"+string(b.Columns[0].ID)+"/delete", form("move_to", "")),
			http.StatusBadRequest, "at least one column")
	})

	t.Run("a column from another board cannot be reached through this one", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		other, err := e.svc.CreateBoard(ctx, "Other", "other", []string{"Theirs"})
		if err != nil {
			t.Fatal(err)
		}
		id := string(other.Columns[0].ID)
		want(t, post(e, "/b/demo/columns/"+id, form("name", "mine", "wip_limit", "")), http.StatusNotFound)
		want(t, post(e, "/b/demo/columns/"+id+"/delete", form("move_to", "")), http.StatusNotFound)
		want(t, post(e, "/b/demo/columns/"+id+"/move", form("direction", "up")), http.StatusNotFound)

		still, _ := e.svc.Board(ctx, "other")
		if still.Columns[0].Name != "Theirs" {
			t.Errorf("the other board's column is now %q", still.Columns[0].Name)
		}
	})
}

func TestVersionEndpoint(t *testing.T) {
	t.Run("it reports the build it was given", func(t *testing.T) {
		svc := service.New(memory.New())
		h := New(svc, nil, nil, WithBuild("1.2.3", "abcdef0"))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/version", nil))
		want(t, rr, http.StatusOK, "1.2.3", "abcdef0")
	})

	t.Run("an unbuilt binary says so rather than inventing one", func(t *testing.T) {
		e := seeded(t)
		want(t, e.do(http.MethodGet, "/version", nil), http.StatusOK, "unknown")
	})
}

func TestTheAddFormHasOneColumnSelect(t *testing.T) {
	e := seeded(t)
	body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
	// The add-card form used to render its own column select above the one in
	// card_fields. Two controls with the same name meant the browser sent
	// whichever came last, so the visible choice was not always the one used.
	if n := strings.Count(body, `name="column"`); n != 1 {
		t.Errorf("the board renders %d column selects, want 1", n)
	}
}

func TestTheCardFormIsInTwoParts(t *testing.T) {
	e := seeded(t)
	body := e.do(http.MethodGet, "/cards/"+string(e.card.ID)+"/edit", nil).Body.String()
	// The split is what the stylesheet grids on a wide screen. Below 1024px
	// the grid is not applied and it is the single column it always was.
	for _, part := range []string{"card-fields__main", "card-fields__side"} {
		if !strings.Contains(body, part) {
			t.Errorf("the edit form has no %s, so it cannot be laid out in two columns", part)
		}
	}
}

func TestReadableOn(t *testing.T) {
	tests := []struct {
		name string
		bg   string
		want string
	}{
		// White on the yellow of the palette is what prompted this.
		{"palette yellow takes dark text", "#eab308", "#111827"},
		{"palette red takes white", "#ef4444", "#ffffff"},
		{"palette blue takes white", "#3b82f6", "#ffffff"},
		{"palette grey takes white", "#6b7280", "#ffffff"},
		{"white takes dark text", "#ffffff", "#111827"},
		{"black takes white", "#000000", "#ffffff"},
		{"a three-digit colour is doubled, not padded", "#ff0", "#111827"},
		{"surrounding space is not a parse failure", "  #eab308  ", "#111827"},
		// Anything unreadable falls back to what labels had before.
		{"an empty colour", "", "#ffffff"},
		{"not a colour at all", "chartreuse", "#ffffff"},
		{"the wrong number of digits", "#abcd", "#ffffff"},
		{"not hexadecimal", "#gggggg", "#ffffff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readableOn(tt.bg); got != tt.want {
				t.Errorf("readableOn(%q) = %q, want %q", tt.bg, got, tt.want)
			}
		})
	}
}

func TestLabelsAreLegibleOnACard(t *testing.T) {
	e := newEnv(t, nil, nil)
	ctx := context.Background()
	b, err := e.svc.CreateBoard(ctx, "Legible", "legible", nil)
	if err != nil {
		t.Fatal(err)
	}
	yellow, err := e.svc.CreateLabel(ctx, b.ID, "sunshine", "#eab308")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.svc.CreateLabel(ctx, b.ID, "unpainted", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{
		Title: "tagged", Labels: []model.ID{yellow.ID, plain.ID},
	}); err != nil {
		t.Fatal(err)
	}

	body := e.do(http.MethodGet, "/b/legible", nil).Body.String()
	// White on that yellow is close to unreadable; the text colour is chosen
	// from the background rather than fixed.
	if !strings.Contains(body, "background-color: #eab308; color: #111827") {
		t.Error("the yellow label did not get dark text")
	}
	// A label with no colour has to be more than grey on grey.
	if !strings.Contains(body, "ring-1 ring-slate-400") {
		t.Error("an uncoloured label has no outline, so it disappears into the card")
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("the colour was refused by the template's CSS escaper")
	}
}

func TestALabelChipSearchesForItsOwnLabel(t *testing.T) {
	setup := func(t *testing.T) *env {
		t.Helper()
		e := seeded(t) // "First card" carries the label "bug"
		ctx := context.Background()
		b, err := e.svc.Board(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		review, err := e.svc.CreateLabel(ctx, b.ID, "in review", "#22c55e")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID,
			service.CardInput{Title: "Second card", Labels: []model.ID{review.ID}}); err != nil {
			t.Fatal(err)
		}
		return e
	}

	t.Run("the chip on the card face links to the board's own search", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `href="/b/demo?q=label:%22bug%22"`) {
			t.Errorf("the bug chip is not a search link:\n%s", body)
		}
		// A drag that begins on a chip has to move the card. Without this the
		// browser drags the link instead and the card stays where it was.
		if !strings.Contains(body, `draggable="false"`) {
			t.Error("the chip link is draggable, which takes the drag away from the card")
		}
		if strings.Contains(body, "ZgotmplZ") {
			t.Error("the link was refused by the template's URL escaper")
		}
	})

	t.Run("the chip carries an x that takes the label off the card", func(t *testing.T) {
		e := setup(t)
		id := string(e.card.ID)
		label := string(e.board.Labels[0].ID)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `hx-post="/cards/`+id+`/labels/`+label+`/toggle"`) {
			t.Errorf("the chip has no x:\n%s", body)
		}

		// And it does the same thing the panel's toggle does, so the label
		// leaves this card and stays on the board.
		rr := e.do(http.MethodPost, "/cards/"+id+"/labels/"+label+"/toggle", nil, "HX-Request", "true")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), `q=label:%22bug%22`) {
			t.Error("the chip is still on the card the label was taken off")
		}
		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Labels) != 2 {
			t.Errorf("the board has %d of its 2 labels, so the x deleted one instead of unlinking it", len(b.Labels))
		}
	})

	t.Run("following it returns the labelled card and nothing else", func(t *testing.T) {
		e := setup(t)
		body := e.do(http.MethodGet, `/b/demo?q=label:%22bug%22`, nil).Body.String()
		if !strings.Contains(body, "First card") {
			t.Error("the labelled card is not in the results")
		}
		if strings.Contains(body, "Second card") {
			t.Error("a card that does not carry the label was returned too")
		}
	})

	t.Run("a name with a space in it is quoted", func(t *testing.T) {
		e := setup(t)
		const link = `/b/demo?q=label:%22in%20review%22`
		board := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(board, `href="`+link+`"`) {
			t.Fatalf("the two-word label was not quoted in its link:\n%s", board)
		}
		// Unquoted, this splits into two terms and finds nothing. Typing it by
		// hand is the search that was reported as broken.
		body := e.do(http.MethodGet, link, nil).Body.String()
		if !strings.Contains(body, "Second card") {
			t.Error("the link for a two-word label found nothing")
		}
		if strings.Contains(body, "First card") {
			t.Error("the two-word label matched a card carrying a different one")
		}
	})

	t.Run("a chip in the archive keeps you in the archive", func(t *testing.T) {
		e := setup(t)
		if err := e.svc.ArchiveCard(context.Background(), e.card.ID); err != nil {
			t.Fatal(err)
		}
		body := e.do(http.MethodGet, "/b/demo/archive", nil).Body.String()
		if !strings.Contains(body, `href="/b/demo?q=is:archived%20label:%22bug%22"`) {
			t.Errorf("the archive's chip drops is:archived and jumps to the live board:\n%s", body)
		}
	})
}

func TestALabelCanBeDeletedFromTheCardFace(t *testing.T) {
	t.Run("the quick panel offers a delete beside every label", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		label := e.board.Labels[0]
		if !strings.Contains(body, `hx-post="/b/demo/labels/`+string(label.ID)+`/delete"`) {
			t.Errorf("no delete beside the label in the quick panel:\n%s", body)
		}
		// The confirm is the only thing between a mis-click and a label that is
		// gone from every card.
		if !strings.Contains(body, "hx-confirm=") {
			t.Error("the delete asks nothing before it takes the label off every card")
		}
	})

	t.Run("deleting over htmx reloads the board", func(t *testing.T) {
		e := seeded(t)
		label := e.board.Labels[0]
		rr := e.do(http.MethodPost, "/b/demo/labels/"+string(label.ID)+"/delete", nil, "HX-Request", "true")
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rr.Code, rr.Body.String())
		}
		// A fragment cannot express the change: the label leaves every card on
		// the board, not only the one the panel was open on.
		if got := rr.Header().Get("HX-Refresh"); got != "true" {
			t.Errorf("HX-Refresh = %q, want true, so the cards that carried it are redrawn", got)
		}
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if strings.Contains(body, "q=label:%22bug%22") {
			t.Errorf("the deleted label is still on a card:\n%s", body)
		}
	})

	t.Run("the settings page still gets its redirect", func(t *testing.T) {
		e := seeded(t)
		label := e.board.Labels[0]
		rr := e.do(http.MethodPost, "/b/demo/labels/"+string(label.ID)+"/delete", nil)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303: %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Location"); got != "/b/demo/settings" {
			t.Errorf("Location = %q, want the settings page", got)
		}
	})
}

// withAvatarHost points the proxy at a test server. WithAvatars has to be passed
// before it, since it is what builds the proxy this replaces the host on.
func withAvatarHost(host string) Option {
	return func(s *Server) { s.avatars.host = host }
}

func TestAvatarsAreServedFromThisOrigin(t *testing.T) {
	const png = "\x89PNG\r\n\x1a\nnot really a png"
	// The setup returns the board with "First card" assigned to somebody who has
	// a picture, and a count of how often the upstream was actually asked.
	setup := func(t *testing.T, upstream http.HandlerFunc) (*env, *atomic.Int64) {
		t.Helper()
		hits := &atomic.Int64{}
		github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			upstream(w, r)
		}))
		t.Cleanup(github.Close)
		e := seeded(t,
			WithAvatars(map[string]string{"Somebody@Example.com": "sapn95"}),
			withAvatarHost(github.URL))
		if _, err := e.svc.SetCardAssignee(context.Background(), e.card.ID, "somebody@example.com"); err != nil {
			t.Fatal(err)
		}
		return e, hits
	}

	servePNG := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sapn95.png" || r.URL.Query().Get("size") == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, png)
	}

	t.Run("the card points at this origin and not at github", func(t *testing.T) {
		e, _ := setup(t, servePNG)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `src="/avatar/sapn95"`) {
			t.Errorf("the assignee has no picture on the card:\n%s", body)
		}
		// The whole point of the proxy: nothing on the page reaches out to
		// GitHub, so it learns neither the viewer's address nor who is on the
		// board. img-src stays 'self' as well.
		if strings.Contains(body, "github.com") {
			t.Error("the page loads something from github.com")
		}
		// The initials stay behind the picture, which is what a failed load
		// falls back to.
		if !strings.Contains(body, "alt=\"\"") || !strings.Contains(body, "object-cover") {
			t.Error("the picture is not laid over the initials bubble")
		}
	})

	t.Run("a picture is fetched once and then cached", func(t *testing.T) {
		e, hits := setup(t, servePNG)
		for range 3 {
			rr := e.do(http.MethodGet, "/avatar/sapn95", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Type"); got != "image/png" {
				t.Errorf("Content-Type = %q", got)
			}
			if rr.Body.String() != png {
				t.Error("the bytes served are not the ones fetched")
			}
			// no-store would mean one request per card per render.
			if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") || !strings.Contains(got, "private") {
				t.Errorf("Cache-Control = %q, want it privately cacheable", got)
			}
		}
		if hits.Load() != 1 {
			t.Errorf("the upstream was asked %d times for one picture", hits.Load())
		}
	})

	t.Run("a login nobody configured is not fetched at all", func(t *testing.T) {
		e, hits := setup(t, servePNG)
		// Otherwise this is an open proxy for github.com/<anything>.png.
		if rr := e.do(http.MethodGet, "/avatar/torvalds", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
		if hits.Load() != 0 {
			t.Error("an unconfigured login reached the upstream")
		}
	})

	t.Run("an upstream that says no is answered with 404 and not asked again", func(t *testing.T) {
		e, hits := setup(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", http.StatusNotFound) })
		for range 2 {
			if rr := e.do(http.MethodGet, "/avatar/sapn95", nil); rr.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rr.Code)
			}
		}
		// Without the failure being cached, a card that carries this person is
		// a request to GitHub on every render, forever.
		if hits.Load() != 1 {
			t.Errorf("the failed fetch was retried %d times", hits.Load())
		}
		// And the card still shows the person: the bubble is the initials with
		// the picture over it, so a picture that never arrives changes nothing.
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `title="somebody@example.com"`) {
			t.Errorf("the assignee vanished with their picture:\n%s", body)
		}
	})

	t.Run("the signed-in person has one in the header too", func(t *testing.T) {
		e, _ := setup(t, servePNG)
		// The card face was the first place this landed and for a while the
		// only one, so the viewer's own bubble in the header kept its initials.
		// Its classes are the anchor: no card renders that combination.
		body := e.doAs("Somebody@Example.com", http.MethodGet, "/b/demo", nil).Body.String()
		const bubble = `text-xs font-semibold uppercase`
		i := strings.Index(body, bubble)
		if i < 0 {
			t.Fatalf("the header shows no bubble for the signed-in person:\n%s", body)
		}
		if header := body[i:min(i+400, len(body))]; !strings.Contains(header, `src="/avatar/sapn95"`) {
			t.Errorf("the header bubble is initials only:\n%s", header)
		}
	})

	t.Run("something that is not an image is refused", func(t *testing.T) {
		e, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>login page</html>")
		})
		rr := e.do(http.MethodGet, "/avatar/sapn95", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if strings.Contains(rr.Body.String(), "login page") {
			t.Error("the upstream's HTML was passed through")
		}
	})

	t.Run("without the option there are no pictures and no route", func(t *testing.T) {
		e := seeded(t)
		if _, err := e.svc.SetCardAssignee(context.Background(), e.card.ID, "somebody@example.com"); err != nil {
			t.Fatal(err)
		}
		if rr := e.do(http.MethodGet, "/avatar/sapn95", nil); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 with the feature off", rr.Code)
		}
		if body := e.do(http.MethodGet, "/b/demo", nil).Body.String(); strings.Contains(body, "/avatar/") {
			t.Error("a picture is rendered although none is configured")
		}
	})
}

func TestAssetURLsCarryAVersion(t *testing.T) {
	e := seeded(t)
	body := e.do(http.MethodGet, "/b/demo", nil).Body.String()

	// Every asset the page pulls has to be busted, not just app.js: the
	// stylesheet and the vendored scripts change with a release too.
	for _, name := range []string{"app.js", "app.css", "logo.svg",
		"vendor/htmx/htmx.min.js", "vendor/sortablejs/Sortable.min.js"} {
		if !strings.Contains(body, "/assets/"+name+"?v=") {
			t.Errorf("%s is referenced without a version, so a browser can keep yesterday's copy", name)
		}
	}
	// And nothing is left on a bare URL that would be cached for a year.
	if strings.Contains(body, `"/assets/app.js"`) {
		t.Error("app.js is still referenced unversioned somewhere")
	}
}

func TestAssetsAreCachedForeverAtAVersionedURL(t *testing.T) {
	e := seeded(t)
	rr := e.do(http.MethodGet, "/assets/app.js?v="+assets.Version(), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// immutable is only honest because the URL changes with the content.
	if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want it immutable", got)
	}
}

func TestTheAssetVersionFollowsTheContent(t *testing.T) {
	v := assets.Version()
	if len(v) != 12 {
		t.Fatalf("version = %q, want twelve hex characters", v)
	}
	if v != assets.Version() {
		t.Error("the version is not stable between calls")
	}
}

func TestBoardLayout(t *testing.T) {
	post := func(e *env, path string, body io.Reader) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, path, body, "Content-Type", "application/x-www-form-urlencoded",
			"Sec-Fetch-Site", "same-origin")
	}

	t.Run("a board starts as columns", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if strings.Contains(body, `class="kanban-scroller rows"`) {
			t.Error("a new board came up as rows")
		}
		// The button offers the layout it is not in.
		if !strings.Contains(body, `value="rows"`) {
			t.Error("the board does not offer to switch to rows")
		}
	})

	t.Run("switching sticks and the button reverses", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/layout", form("layout", "rows")), http.StatusSeeOther)

		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `class="kanban-scroller rows"`) {
			t.Error("the board did not come back as rows")
		}
		if !strings.Contains(body, `value="columns"`) {
			t.Error("the button does not offer the way back")
		}
		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		// It belongs to the board, not to the browser: a board that reads
		// better as rows reads better as rows for whoever opens it.
		if b.Layout != model.LayoutRows {
			t.Errorf("stored layout = %q, want rows", b.Layout)
		}
	})

	t.Run("an unknown layout is refused and changes nothing", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/layout", form("layout", "spiral")), http.StatusBadRequest)
		b, _ := e.svc.Board(context.Background(), "demo")
		if b.Layout != model.LayoutColumns {
			t.Errorf("layout = %q after a refused change, want it untouched", b.Layout)
		}
	})
}

func TestQuickEditOnTheCardFace(t *testing.T) {
	const her, him = "her@example.com", "him@example.com"

	post := func(t *testing.T, e *env, user, path string, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// The quick edits answer with the card face, which only means anything
		// to htmx; without the header they redirect to the board.
		req.Header.Set("HX-Request", "true")
		if user != "" {
			req = req.WithContext(identity.NewContext(req.Context(), identity.User{Email: user}))
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	assignee := func(t *testing.T, e *env) string {
		t.Helper()
		c, err := e.svc.Card(context.Background(), e.card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.Assignee
	}
	dueDate := func(t *testing.T, e *env) time.Time {
		t.Helper()
		c, err := e.svc.Card(context.Background(), e.card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.DueDate
	}

	t.Run("the card face carries both menus", func(t *testing.T) {
		e := seeded(t)
		card, label := string(e.card.ID), string(e.board.Labels[0].ID)
		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK,
			`data-panel="assignee-`+card,
			`data-panel="labels-`+card,
			`id="assignee-`+card,
			`id="labels-`+card,
			`hx-post="/cards/`+card+`/labels/`+label+`/toggle"`)
	})

	t.Run("the menu offers the people already on the board, the viewer first", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		for _, who := range []string{him, "zoe@example.com"} {
			if _, err := e.svc.CreateCard(ctx, e.board.ID, e.board.Columns[0].ID,
				service.CardInput{Title: "for " + who, Assignee: who}); err != nil {
				t.Fatal(err)
			}
		}
		body := e.doAs(her, http.MethodGet, "/b/demo", nil).Body.String()

		// The viewer is offered by name, so their own address is not written
		// into every card on the board.
		if !strings.Contains(body, `value="@me"`) {
			t.Error("the menu does not offer the viewer")
		}
		himAt := strings.Index(body, `value="`+him+`"`)
		zoeAt := strings.Index(body, `value="zoe@example.com"`)
		if himAt < 0 || zoeAt < 0 {
			t.Fatalf("the menu is missing somebody: him=%d zoe=%d", himAt, zoeAt)
		}
		// Alphabetical below the viewer, so the menu does not reshuffle itself
		// every time a card moves.
		if himAt > zoeAt {
			t.Error("the people are not in alphabetical order")
		}
	})

	t.Run("picking somebody assigns them and redraws the card", func(t *testing.T) {
		e := seeded(t)
		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/assignee", form("assignee", him)),
			http.StatusOK, `id="card-`+string(e.card.ID), him)
		if got := assignee(t, e); got != him {
			t.Errorf("assignee = %q, want %q", got, him)
		}
		// One field, and nothing else: that is the whole reason this is not the
		// edit form with most of its inputs missing.
		c, _ := e.svc.Card(context.Background(), e.card.ID)
		if c.Title != "First card" || len(c.Labels) != 1 || len(c.Subtasks) != 2 {
			t.Errorf("the quick edit disturbed the rest of the card: %+v", c)
		}
	})

	t.Run("the server resolves the viewer, and refuses to guess", func(t *testing.T) {
		e := seeded(t)
		want(t, post(t, e, her, "/cards/"+string(e.card.ID)+"/assignee", form("assignee", "@me")),
			http.StatusOK, her)
		if got := assignee(t, e); got != her {
			t.Errorf("assignee = %q, want %q", got, her)
		}

		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/assignee", form("assignee", "@me")),
			http.StatusForbidden)
		if got := assignee(t, e); got != her {
			t.Errorf("assignee = %q after a refused @me, want it untouched", got)
		}
	})

	t.Run("unassigning clears it", func(t *testing.T) {
		e := seeded(t)
		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/assignee", form("assignee", him)), http.StatusOK)
		body := post(t, e, "", "/cards/"+string(e.card.ID)+"/assignee", form("assignee", "")).Body.String()
		if strings.Contains(body, him) {
			t.Error("the assignee survived being cleared from the card face")
		}
		if got := assignee(t, e); got != "" {
			t.Errorf("assignee = %q, want empty", got)
		}
	})

	t.Run("the date is set from the card face, whether or not it has one", func(t *testing.T) {
		e := seeded(t)
		blank, err := e.svc.CreateCard(context.Background(), e.board.ID, e.board.Columns[0].ID,
			service.CardInput{Title: "no date yet"})
		if err != nil {
			t.Fatal(err)
		}
		// The chip with a date opens on a double click, because a single click
		// on a card belongs to ticking it and to starting a drag. A card
		// without one has nothing to double-click, so it gets the dashed chip
		// the other quick edits use.
		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK,
			`class="dbl-toggle select-none`,
			`data-panel="due-`+string(e.card.ID),
			`id="due-`+string(e.card.ID),
			`type="date" name="due_date" value="2026-09-01"`,
			`data-panel="due-`+string(blank.ID))
	})

	t.Run("picking a date sets it and redraws the card", func(t *testing.T) {
		e := seeded(t)
		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/due", form("due_date", "2026-12-24")),
			http.StatusOK, `id="card-`+string(e.card.ID), "24 Dec 2026")
		if got := dueDate(t, e).Format("2006-01-02"); got != "2026-12-24" {
			t.Errorf("due date = %s, want 2026-12-24", got)
		}
		// One field, same as the assignee: the rest of the card is not sent.
		c, _ := e.svc.Card(context.Background(), e.card.ID)
		if c.Title != "First card" || len(c.Labels) != 1 || len(c.Subtasks) != 2 {
			t.Errorf("the picker disturbed the rest of the card: %+v", c)
		}
	})

	t.Run("clearing takes the date off and leaves a way back", func(t *testing.T) {
		e := seeded(t)
		body := post(t, e, "", "/cards/"+string(e.card.ID)+"/due", form("due_date", "")).Body.String()
		if strings.Contains(body, "Sep 2026") {
			t.Error("the date survived being cleared from the card face")
		}
		if !strings.Contains(body, `data-panel="due-`+string(e.card.ID)) {
			t.Error("the cleared card came back with no way to set a date again")
		}
		if got := dueDate(t, e); !got.IsZero() {
			t.Errorf("due date = %v, want none", got)
		}
	})

	t.Run("a date the picker could not have sent is refused", func(t *testing.T) {
		e := seeded(t)
		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/due", form("due_date", "24.12.2026")),
			http.StatusBadRequest)
		if got := dueDate(t, e).Format("2006-01-02"); got != "2026-09-01" {
			t.Errorf("due date = %s after a refused post, want it untouched", got)
		}
	})

	t.Run("a label toggles off and on again", func(t *testing.T) {
		e := seeded(t)
		card, label := string(e.card.ID), string(e.board.Labels[0].ID)
		labels := func() []model.ID {
			c, _ := e.svc.Card(context.Background(), e.card.ID)
			return c.Labels
		}

		// The seeded card already carries it, so the first click takes it off.
		want(t, post(t, e, "", "/cards/"+card+"/labels/"+label+"/toggle", nil), http.StatusOK)
		if got := labels(); len(got) != 0 {
			t.Errorf("labels = %v, want none", got)
		}
		want(t, post(t, e, "", "/cards/"+card+"/labels/"+label+"/toggle", nil), http.StatusOK, "bug")
		if got := labels(); len(got) != 1 || got[0] != model.ID(label) {
			t.Errorf("labels = %v, want the bug label back", got)
		}
	})

	t.Run("a label from another board is refused", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		other, err := e.svc.CreateBoard(ctx, "Other", "other", nil)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := e.svc.CreateLabel(ctx, other.ID, "theirs", "#00f")
		if err != nil {
			t.Fatal(err)
		}
		want(t, post(t, e, "", "/cards/"+string(e.card.ID)+"/labels/"+string(theirs.ID)+"/toggle", nil),
			http.StatusNotFound)
		c, _ := e.svc.Card(ctx, e.card.ID)
		if len(c.Labels) != 1 {
			t.Errorf("labels = %v, want the refused toggle to have changed nothing", c.Labels)
		}
	})

	t.Run("without htmx a quick edit lands back on the board", func(t *testing.T) {
		e := seeded(t)
		rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/assignee", form("assignee", him))
		want(t, rr, http.StatusSeeOther)
		if got := rr.Header().Get("Location"); got != "/b/demo" {
			t.Errorf("Location = %q, want /b/demo", got)
		}
	})
}

// TestFooterIsOnEveryPage covers the line at the bottom of the layout. The
// version is the point of it: the Pi rolls out by tag, and before this the only
// way to tell which build a page came from was to curl /version.
func TestFooterIsOnEveryPage(t *testing.T) {
	e := seeded(t, WithBuild("2.2.1", "abcdef0"))
	// A second board, so the index is a page rather than a redirect to the only
	// board there is.
	if _, err := e.svc.CreateBoard(context.Background(), "Other", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/b/demo", "/b/demo/settings", "/b/demo/archive"} {
		t.Run(path, func(t *testing.T) {
			want(t, e.do(http.MethodGet, path, nil), http.StatusOK,
				"<footer", `href="/"`, `href="/version"`, "kanban 2.2.1", "All boards")
		})
	}

	t.Run("an unbuilt binary says dev rather than nothing", func(t *testing.T) {
		e := seeded(t)
		want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK, "kanban dev")
	})

	t.Run("the API is linked only where it is mounted", func(t *testing.T) {
		off := seeded(t)
		if body := off.do(http.MethodGet, "/b/demo", nil).Body.String(); strings.Contains(body, `href="/api/v1/"`) {
			t.Error("the footer links to an API this deployment does not serve")
		}
		on := seeded(t, WithAPI(http.NotFoundHandler()))
		want(t, on.do(http.MethodGet, "/b/demo", nil), http.StatusOK, `href="/api/v1/"`)
	})
}

// TestSLAThroughTheWeb is the response-time promise as the settings form and the
// card face see it.
func TestSLAThroughTheWeb(t *testing.T) {
	post := func(e *env, path string, body io.Reader) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, path, body, "Content-Type", "application/x-www-form-urlencoded",
			"Sec-Fetch-Site", "same-origin")
	}
	// The office week as the form posts it, with anything named in kv replacing
	// the default for that field. days may be named more than once, and naming
	// it once replaces the whole week rather than adding to it.
	week := func(kv ...string) io.Reader {
		v := url.Values{
			"on": {"1"}, "start": {"08:00"}, "end": {"17:00"}, "zone": {"Europe/Zurich"},
			"days": {"mon", "tue", "wed", "thu", "fri"},
		}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Del(kv[i])
		}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return strings.NewReader(v.Encode())
	}

	t.Run("a board starts with no promise", func(t *testing.T) {
		e := seeded(t)
		want(t, e.do(http.MethodGet, "/b/demo/settings", nil), http.StatusOK,
			`action="/b/demo/sla"`, `name="response_hours"`, `name="days"`, `value="mon"`,
			`name="on"`, "Off. No card carries a clock.")
		// Nothing on the board carries a clock either.
		if body := e.do(http.MethodGet, "/b/demo", nil).Body.String(); strings.Contains(body, "bi-stopwatch") {
			t.Error("a board with no promise drew a response-time badge")
		}
	})

	t.Run("the form writes it and the page reads it back", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/sla", week("response_hours", "4")), http.StatusSeeOther)

		b, err := e.svc.Board(context.Background(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		want := model.SLA{ResponseHours: 4, Days: model.MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
		if b.SLA != want {
			t.Fatalf("stored sla = %+v, want %+v", b.SLA, want)
		}
		want2 := []string{"4 office hours, Mon to Fri, 08:00 to 17:00 Europe/Zurich",
			`value="08:00"`, `value="17:00"`, `<option selected>Europe/Zurich</option>`,
			`name="on" value="1" class="sr-only" checked`}
		body := e.do(http.MethodGet, "/b/demo/settings", nil).Body.String()
		for _, s := range want2 {
			if !strings.Contains(body, s) {
				t.Errorf("settings page missing %q", s)
			}
		}
	})

	t.Run("a desk open around the clock", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/sla", week("response_hours", "2", "start", "00:00", "end", "00:00",
			"days", "mon", "days", "tue", "days", "wed", "days", "thu", "days", "fri",
			"days", "sat", "days", "sun")), http.StatusSeeOther)
		b, _ := e.svc.Board(context.Background(), "demo")
		if b.SLA.Start != 0 || b.SLA.End != model.MinutesPerDay || b.SLA.Days != model.AllDays {
			t.Fatalf("stored sla = %+v, want the whole week from midnight to midnight", b.SLA)
		}
		want(t, e.do(http.MethodGet, "/b/demo/settings", nil), http.StatusOK,
			"2 office hours, Mon to Sun, 00:00 to 00:00 Europe/Zurich")
	})

	// The switch is the way off, and it is the only way off: an unticked
	// checkbox sends no field, so the hours in the form are not what decides.
	t.Run("the switch turns it off and the office hours stay", func(t *testing.T) {
		e := seeded(t)
		want(t, post(e, "/b/demo/sla", week("response_hours", "4")), http.StatusSeeOther)

		// The same form, submitted with the switch off: the hours are still in
		// the box and are still ignored.
		want(t, post(e, "/b/demo/sla", week("on", "")), http.StatusSeeOther)
		b, _ := e.svc.Board(context.Background(), "demo")
		if b.SLA.ResponseHours != 0 || b.SLA.Start != 8*60 || b.SLA.Zone != "Europe/Zurich" {
			t.Errorf("stored sla = %+v, want the office hours kept and the clock off", b.SLA)
		}
		body := e.do(http.MethodGet, "/b/demo/settings", nil).Body.String()
		for _, s := range []string{"Off. No card carries a clock.", `value="08:00"`, `value="8"`} {
			if !strings.Contains(body, s) {
				t.Errorf("settings page missing %q", s)
			}
		}
		if strings.Contains(body, `name="on" value="1" class="sr-only" checked`) {
			t.Error("the switch still reads as on")
		}

		// And back on, with the number the form was already showing.
		want(t, post(e, "/b/demo/sla", week("response_hours", "8")), http.StatusSeeOther)
		if b, _ = e.svc.Board(context.Background(), "demo"); b.SLA.ResponseHours != 8 {
			t.Errorf("stored sla = %+v, want the promise back at 8", b.SLA)
		}
	})

	// The picker is a select over the whole zone database rather than a text
	// field with six suggestions, which is what made it look like one zone.
	t.Run("the zone picker offers every zone the binary can load", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo/settings", nil).Body.String()
		for _, s := range []string{`<select name="zone"`, `<optgroup label="Europe">`,
			`<optgroup label="Pacific">`, "<option>Europe/Zurich</option>", "<option>Pacific/Auckland</option>"} {
			if !strings.Contains(body, s) {
				t.Errorf("the zone picker is missing %q", s)
			}
		}
		if n := strings.Count(body, "<option"); n < model.ZoneCount {
			t.Errorf("the picker draws %d options, want at least the %d zones", n, model.ZoneCount)
		}
		if strings.Contains(body, "<datalist") {
			t.Error("the datalist that filtered itself down to one entry is still there")
		}
	})

	// A zone set through the API that the generated list does not carry is kept
	// on offer, so saving the form does not quietly change it.
	t.Run("a zone from somewhere else is not dropped", func(t *testing.T) {
		e := seeded(t)
		if err := e.svc.SetBoardSLA(context.Background(), e.board.ID, model.SLA{
			ResponseHours: 4, Days: model.MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "US/Eastern",
		}); err != nil {
			t.Fatal(err)
		}
		want(t, e.do(http.MethodGet, "/b/demo/settings", nil), http.StatusOK,
			`<optgroup label="Set on this board">`, "<option selected>US/Eastern</option>")
	})

	t.Run("what the form refuses", func(t *testing.T) {
		for _, tc := range []struct {
			name, says string
			body       io.Reader
		}{
			{"a zone nobody can load", "IANA time zone", week("response_hours", "4", "zone", "Mars/Olympus")},
			{"hours that are not a number", "whole number", week("response_hours", "soon")},
			{"the switch on with no hours behind it", "at least one office hour", week("response_hours", "0")},
			{"a clock reading that is not one", "not a time of day", week("response_hours", "4", "start", "half eight")},
			{"an end before the start", "must end after", week("response_hours", "4", "start", "17:00", "end", "08:00")},
			{"a promise on no day at all", "at least one day",
				form("on", "1", "response_hours", "4", "start", "08:00", "end", "17:00")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := seeded(t)
				rr := post(e, "/b/demo/sla", tc.body)
				// The settings page comes back with the reason on it rather
				// than a bare 400, because the form it belongs to is there.
				want(t, rr, http.StatusBadRequest, tc.says, `action="/b/demo/sla"`)
				b, _ := e.svc.Board(context.Background(), "demo")
				if b.SLA.Enabled() {
					t.Errorf("a refused form switched the clock on: %+v", b.SLA)
				}
			})
		}
	})

	t.Run("the badge grades a card against the promise", func(t *testing.T) {
		// A desk open around the clock, so the badge does not depend on which
		// weekday the fixed test clock happens to be.
		open := func(e *env) {
			t.Helper()
			if err := e.svc.SetBoardSLA(context.Background(), e.board.ID, model.SLA{
				ResponseHours: 4, Days: model.AllDays, End: model.MinutesPerDay,
			}); err != nil {
				t.Fatal(err)
			}
		}

		t.Run("a card touched just now", func(t *testing.T) {
			e := seeded(t)
			open(e)
			want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK,
				"bi-stopwatch", "4h left", "bg-emerald-100", "Answer by")
		})

		t.Run("an hour from the deadline", func(t *testing.T) {
			e := seeded(t, WithClock(func() time.Time { return today.Add(3 * time.Hour) }))
			open(e)
			want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK, "1h left", "bg-amber-100")
		})

		t.Run("two days past it", func(t *testing.T) {
			e := seeded(t, WithClock(func() time.Time { return today.Add(48 * time.Hour) }))
			open(e)
			want(t, e.do(http.MethodGet, "/b/demo", nil), http.StatusOK,
				"44h over", "bg-rose-100", "The response time ran out on")
		})

		t.Run("the clock does not run in a column that stops it", func(t *testing.T) {
			e := seeded(t, WithClock(func() time.Time { return today.Add(48 * time.Hour) }))
			open(e)
			ctx := context.Background()
			col := e.board.Columns[0]
			if err := e.svc.UpdateColumn(ctx, col.ID, col.Name, 0, true); err != nil {
				t.Fatal(err)
			}
			if body := e.do(http.MethodGet, "/b/demo", nil).Body.String(); strings.Contains(body, "bi-stopwatch") {
				t.Error("a card in a column where the clock stops still carries a badge")
			}
		})
	})
}

// The count on a column header, the bar under it and the notice under that are
// the WIP limit's whole interface, and every write that changes a count has to
// bring them along or the board disagrees with itself until the next reload.
func TestColumnHeadersComeBackWithEveryWrite(t *testing.T) {
	oob := func(t *testing.T, rr *httptest.ResponseRecorder, cols ...model.ID) {
		t.Helper()
		body := rr.Body.String()
		for _, c := range cols {
			if !strings.Contains(body, `id="colhead-`+string(c)+`" hx-swap-oob="true"`) {
				t.Errorf("the response does not redraw column %s:\n%s", c, body)
			}
		}
	}

	t.Run("adding a card", func(t *testing.T) {
		e := seeded(t)
		todo := e.board.Columns[0].ID
		rr := e.do(http.MethodPost, "/b/demo/cards", form("title", "new", "column", string(todo)), "HX-Request", "true")
		want(t, rr, http.StatusOK, "new")
		oob(t, rr, todo)
	})

	t.Run("archiving one", func(t *testing.T) {
		e := seeded(t)
		rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/archive", nil)
		want(t, rr, http.StatusOK)
		oob(t, rr, e.board.Columns[0].ID)
	})

	t.Run("deleting one", func(t *testing.T) {
		e := seeded(t)
		rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/delete", nil)
		want(t, rr, http.StatusOK)
		oob(t, rr, e.board.Columns[0].ID)
	})

	t.Run("a full column says so in the header it sends back", func(t *testing.T) {
		e := seeded(t)
		ctx := context.Background()
		todo := e.board.Columns[0]
		if err := e.svc.UpdateColumn(ctx, todo.ID, todo.Name, 1, false); err != nil {
			t.Fatal(err)
		}
		// The card that is already there is archived and put back, so the
		// header arrives once under the limit and once at it.
		rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/archive", nil)
		if body := rr.Body.String(); strings.Contains(body, "At the limit.") {
			t.Error("an empty column reports itself full")
		}
		second, err := e.svc.CreateCard(ctx, e.board.ID, todo.ID, service.CardInput{Title: "second"})
		if err != nil {
			t.Fatal(err)
		}
		rr = e.do(http.MethodPost, "/cards/"+string(second.ID)+"/archive", nil)
		want(t, rr, http.StatusOK)
		if body := rr.Body.String(); strings.Contains(body, "0 / 1") == false {
			t.Errorf("the header does not report the count against the limit:\n%s", body)
		}
	})
}

// The Add Subtask button asks the server for the row rather than building one,
// so a checklist line is written down once.
func TestSubtaskRowComesFromTheServer(t *testing.T) {
	e := seeded(t)
	rr := e.do(http.MethodGet, "/subtask-row", nil)
	want(t, rr, http.StatusOK, "subtask-row", "subtask-text", "subtask-complete", "remove-subtask-btn")
	if body := rr.Body.String(); strings.Contains(body, "checked") {
		t.Error("a row that nobody has typed in yet arrives ticked")
	}
	// The same markup the edit form renders for a subtask a card already has.
	edit := e.do(http.MethodGet, "/cards/"+string(e.card.ID)+"/edit", nil).Body.String()
	if !strings.Contains(edit, `class="subtask-row flex items-center space-x-2 mb-2"`) {
		t.Error("the edit form and /subtask-row have drifted apart")
	}
	// Both forms carry the button, and in both the container it appends to is
	// the element right before it, which is what `previous` resolves against.
	for name, body := range map[string]string{
		"the edit form": edit,
		"the add form":  e.do(http.MethodGet, "/b/demo", nil).Body.String(),
	} {
		i := strings.Index(body, `class="subtasks-container`)
		j := strings.Index(body, `hx-get="/subtask-row" hx-target="previous .subtasks-container"`)
		if i < 0 || j < 0 || j < i {
			t.Errorf("%s does not have the Add Subtask button after its container (container %d, button %d)", name, i, j)
		}
	}
}

// What makes the board installable, and what happens when it is installed and
// the server is not there. All three are at the root on purpose: a service
// worker controls only what is under the path it came from, and a manifest's
// scope defaults to its own directory, so from /assets/ both would cover the
// assets and nothing else.
func TestTheBoardCanBeInstalled(t *testing.T) {
	e := seeded(t)

	t.Run("the manifest", func(t *testing.T) {
		rr := e.do(http.MethodGet, "/manifest.webmanifest", nil)
		want(t, rr, http.StatusOK)
		if got := rr.Header().Get("Content-Type"); got != "application/manifest+json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache; a stale manifest is an app that will not update", got)
		}
		var m struct {
			Name     string `json:"name"`
			StartURL string `json:"start_url"`
			Scope    string `json:"scope"`
			Display  string `json:"display"`
			Theme    string `json:"theme_color"`
			Icons    []struct {
				Src, Sizes, Type, Purpose string
			} `json:"icons"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatalf("the manifest is not JSON: %v", err)
		}
		if m.Scope != "/" || m.StartURL != "/" || m.Display != "standalone" {
			t.Errorf("manifest = %+v, want the whole origin in standalone", m)
		}
		var maskable bool
		for _, i := range m.Icons {
			if i.Purpose == "maskable" {
				maskable = true
			}
			if !strings.HasPrefix(i.Src, "/") {
				t.Errorf("icon %q is not an absolute path, so it resolves against the manifest", i.Src)
			}
			if rr := e.do(http.MethodGet, i.Src, nil); rr.Code != http.StatusOK {
				t.Errorf("icon %s: %d", i.Src, rr.Code)
			}
		}
		if !maskable {
			t.Error("no maskable icon, so Android draws the square inside its own shape")
		}
	})

	t.Run("the service worker", func(t *testing.T) {
		rr := e.do(http.MethodGet, "/sw.js", nil)
		want(t, rr, http.StatusOK, "addEventListener", "/offline")
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
			t.Errorf("Content-Type = %q; a worker served as anything else is refused", got)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", got)
		}
		// It must not cache the board itself: a cached column is yesterday's
		// work with nothing on the page saying so.
		if body := rr.Body.String(); strings.Contains(body, "cache.addAll") {
			t.Error("the worker caches more than the offline page")
		}
	})

	t.Run("the offline page", func(t *testing.T) {
		want(t, e.do(http.MethodGet, "/offline", nil), http.StatusOK, "No connection", "Try again")
	})

	t.Run("every page says where the manifest is", func(t *testing.T) {
		for _, path := range []string{"/b/demo", "/b/demo/settings", "/offline"} {
			body := e.do(http.MethodGet, path, nil).Body.String()
			for _, s := range []string{`rel="manifest" href="/manifest.webmanifest"`,
				`name="theme-color"`, `rel="apple-touch-icon"`} {
				if !strings.Contains(body, s) {
					t.Errorf("%s is missing %q", path, s)
				}
			}
		}
	})
}

// Getting to another board, and to the form that makes one. Both existed and
// neither was reachable while there was a single board: GET / redirects to it,
// so the footer's way to the list came straight back, and the list is where a
// board is created.
func TestGettingToAnotherBoard(t *testing.T) {
	e := seeded(t)

	t.Run("the list is there even with one board", func(t *testing.T) {
		want(t, e.do(http.MethodGet, "/boards", nil), http.StatusOK,
			`action="/boards"`, "New board name", "Demo")
		// And / still goes to the board, which is what a bookmark wants.
		rr := e.do(http.MethodGet, "/", nil)
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/demo" {
			t.Errorf("GET / = %d to %q, want a redirect to the one board", rr.Code, rr.Header().Get("Location"))
		}
	})

	t.Run("the app bar switches boards", func(t *testing.T) {
		if _, err := e.svc.CreateBoard(context.Background(), "Other", "", nil); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/b/demo", "/b/demo/settings", "/b/demo/archive"} {
			body := e.do(http.MethodGet, path, nil).Body.String()
			for _, s := range []string{`href="/b/other"`, `href="/boards"`, "New board"} {
				if !strings.Contains(body, s) {
					t.Errorf("%s has no way to %q", path, s)
				}
			}
		}
	})

	t.Run("the footer points at the list, not at the redirect", func(t *testing.T) {
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, `<a href="/boards" class="hover:text-slate-600`) {
			t.Error("the footer still sends All boards through the redirect")
		}
	})

	t.Run("a page with no board draws no switcher", func(t *testing.T) {
		body := e.do(http.MethodGet, "/boards", nil).Body.String()
		if strings.Contains(body, "bi-chevron-down") {
			t.Error("the board list offers a switcher to itself")
		}
	})
}

// A service token is a caller and not a person: it gets through a board that
// requires an identity, and "@me" still has nobody to mean.
func TestAMachineCallerHasNoAtMe(t *testing.T) {
	e := seeded(t)
	machine := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req = req.WithContext(identity.NewContext(req.Context(),
			identity.User{Service: "88fe4a3c.access"}))
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		return rr
	}

	t.Run("it can read the board", func(t *testing.T) {
		want(t, machine(http.MethodGet, "/b/demo", nil), http.StatusOK, "First card")
	})

	t.Run("assigning to @me is refused", func(t *testing.T) {
		rr := machine(http.MethodPost, "/cards/"+string(e.card.ID)+"/assignee", form("assignee", "@me"))
		want(t, rr, http.StatusForbidden, "not signed in")
		c, _ := e.svc.Card(context.Background(), e.card.ID)
		if c.Assignee != "" {
			t.Errorf("assignee = %q, want it left alone", c.Assignee)
		}
	})

	t.Run("and so is a bulk assign to @me", func(t *testing.T) {
		body := url.Values{"action": {"assign"}, "target": {"@me"}, "ids": {string(e.card.ID)}}
		want(t, machine(http.MethodPost, "/b/demo/cards/bulk", strings.NewReader(body.Encode())),
			http.StatusForbidden, "not signed in")
	})

	t.Run("a card it writes carries no address", func(t *testing.T) {
		rr := machine(http.MethodPost, "/b/demo/cards", form("title", "from a cron job"))
		if rr.Code != http.StatusSeeOther && rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		cards, _ := e.svc.Cards(context.Background(), e.board.ID)
		for _, c := range cards {
			if c.Title == "from a cron job" && c.Assignee != "" {
				t.Errorf("assignee = %q, want nobody", c.Assignee)
			}
		}
	})
}

// The app bar says who can see the board, because a board behind a sign-in
// knows who is at the keyboard and nothing about who else was let in.
func TestViewerRoster(t *testing.T) {
	roster := []identity.User{
		{Email: "ada@example.com", Name: "Ada Lovelace"},
		{Email: "grace.hopper@example.com"},
	}

	t.Run("every name and address is on the page", func(t *testing.T) {
		e := seeded(t, WithViewers(roster))
		body := e.doAs("ada@example.com", http.MethodGet, "/b/demo", nil).Body.String()
		for _, want := range []string{
			"Who can see this board",
			"Ada Lovelace", "ada@example.com",
			// No name was configured for the second, so the board reads one
			// out of the address rather than printing the local part.
			"Grace Hopper", "grace.hopper@example.com",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the app bar does not mention %q", want)
			}
		}
	})

	t.Run("and says it decides nothing", func(t *testing.T) {
		e := seeded(t, WithViewers(roster))
		body := e.doAs("ada@example.com", http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, "decides who gets in") {
			t.Error("the panel does not say the sign-in is what admits anybody")
		}
	})

	t.Run("no roster, no panel", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if strings.Contains(body, "Who can see this board") {
			t.Error("a board with no roster offers the panel anyway")
		}
	})

	// A board in proxy mode without AUTH_REQUIRED still draws a page for a
	// caller it could not identify. The roster is a list of people's addresses
	// and that caller does not get it.
	t.Run("nobody signed in, no roster", func(t *testing.T) {
		e := seeded(t, WithViewers(roster))
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		for _, leaked := range []string{"Ada Lovelace", "ada@example.com", "Who can see this board"} {
			if strings.Contains(body, leaked) {
				t.Errorf("an unidentified caller was shown %q", leaked)
			}
		}
	})
}

// A board was the one thing that could be made here and never removed.
func TestDeleteBoard(t *testing.T) {
	newEnv := func(t *testing.T) *env {
		t.Helper()
		e := seeded(t)
		if _, err := e.svc.CreateBoard(context.Background(), "Scratch", "scratch", nil); err != nil {
			t.Fatal(err)
		}
		return e
	}

	t.Run("the name typed back deletes it", func(t *testing.T) {
		e := newEnv(t)
		rr := e.do(http.MethodPost, "/b/scratch/delete", form("confirm", "Scratch"))
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/boards" {
			t.Fatalf("got %d to %q, want 303 to /boards", rr.Code, rr.Header().Get("Location"))
		}
		boards, err := e.svc.Boards(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range boards {
			if b.Slug == "scratch" {
				t.Error("the board is still there")
			}
		}
	})

	t.Run("case is forgiven, a wrong name is not", func(t *testing.T) {
		e := newEnv(t)
		if rr := e.do(http.MethodPost, "/b/scratch/delete", form("confirm", "  scRATCH ")); rr.Code != http.StatusSeeOther {
			t.Errorf("got %d for the name in another case, want 303", rr.Code)
		}
		e = newEnv(t)
		rr := e.do(http.MethodPost, "/b/scratch/delete", form("confirm", "Scratchh"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("got %d for a wrong name, want 400", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "type its name exactly") {
			t.Error("the page does not say why nothing happened")
		}
		if _, err := e.svc.Board(context.Background(), "scratch"); err != nil {
			t.Errorf("the board went anyway: %v", err)
		}
	})

	t.Run("an empty confirmation is not a match either", func(t *testing.T) {
		e := newEnv(t)
		if rr := e.do(http.MethodPost, "/b/scratch/delete", form("confirm", "")); rr.Code != http.StatusBadRequest {
			t.Errorf("got %d for an empty confirmation, want 400", rr.Code)
		}
		if _, err := e.svc.Board(context.Background(), "scratch"); err != nil {
			t.Errorf("the board went anyway: %v", err)
		}
	})

	t.Run("the cards go with it", func(t *testing.T) {
		e := newEnv(t)
		b, err := e.svc.Board(context.Background(), "scratch")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CreateCard(context.Background(), b.ID, b.Columns[0].ID,
			service.CardInput{Title: "goes with the board"}); err != nil {
			t.Fatal(err)
		}
		if rr := e.do(http.MethodPost, "/b/scratch/delete", form("confirm", "Scratch")); rr.Code != http.StatusSeeOther {
			t.Fatalf("got %d, want 303", rr.Code)
		}
		if _, err := e.svc.Cards(context.Background(), b.ID); err == nil {
			// A store that answers for a deleted board would leave orphans.
			if cards, _ := e.svc.Cards(context.Background(), b.ID); len(cards) != 0 {
				t.Errorf("%d card(s) outlived the board", len(cards))
			}
		}
	})

	t.Run("the list offers a bin for every board", func(t *testing.T) {
		e := newEnv(t)
		body := e.do(http.MethodGet, "/boards", nil).Body.String()
		if n := strings.Count(body, "/delete"); n != 2 {
			t.Errorf("%d delete forms on a page with 2 boards", n)
		}
		if !strings.Contains(body, "There is no undo") {
			t.Error("the confirmation does not say the board is gone for good")
		}
	})
}

// Ticking a checklist line from the board, without the edit form.
func TestToggleSubtaskFromTheCardFace(t *testing.T) {
	t.Run("the card comes back with the line ticked", func(t *testing.T) {
		e := seeded(t)
		id := e.card.Subtasks[1].ID // "sub two", not done
		rr := e.do(http.MethodPost,
			"/cards/"+string(e.card.ID)+"/subtasks/"+string(id)+"/toggle", nil, "HX-Request", "true")
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		c, err := e.svc.Card(context.Background(), e.card.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Subtasks[1].Done {
			t.Error("the subtask is still open")
		}
		if !strings.Contains(rr.Body.String(), "2/2") {
			t.Errorf("the progress on the returned card face was not redrawn:\n%s", rr.Body.String())
		}
	})

	t.Run("and untick is the same call", func(t *testing.T) {
		e := seeded(t)
		id := e.card.Subtasks[0].ID // "sub one", already done
		e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/subtasks/"+string(id)+"/toggle", nil)
		c, _ := e.svc.Card(context.Background(), e.card.ID)
		if c.Subtasks[0].Done {
			t.Error("a done subtask stayed done")
		}
	})

	t.Run("without htmx it is a redirect to the board", func(t *testing.T) {
		e := seeded(t)
		id := e.card.Subtasks[0].ID
		rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/subtasks/"+string(id)+"/toggle", nil)
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/b/demo" {
			t.Errorf("got %d to %q, want 303 to /b/demo", rr.Code, rr.Header().Get("Location"))
		}
	})

	// By id and not by position, so a checklist reordered in another tab cannot
	// tick whatever moved into the slot.
	t.Run("an id the card does not have is a 404", func(t *testing.T) {
		e := seeded(t)
		if rr := e.do(http.MethodPost, "/cards/"+string(e.card.ID)+"/subtasks/nope/toggle", nil); rr.Code != http.StatusNotFound {
			t.Errorf("got %d, want 404", rr.Code)
		}
	})

	t.Run("the row on the card face is a button, not an icon", func(t *testing.T) {
		e := seeded(t)
		body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
		if !strings.Contains(body, "subtask-tick") {
			t.Fatal("the checklist on the card face has no tick control")
		}
		if !strings.Contains(body, `aria-pressed="true"`) || !strings.Contains(body, `aria-pressed="false"`) {
			t.Error("the rows do not say which are ticked")
		}
	})
}
