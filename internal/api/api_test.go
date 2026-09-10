package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
	"kanban/internal/store/memory"
)

var today = time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

// Every request in this file goes through env.do, which looks the answer up in
// openapi.json and validates the body against the schema declared there. So a
// handler that grows a field the document does not have fails here, and so does
// a status code nobody wrote down.

type env struct {
	t     *testing.T
	h     http.Handler
	svc   *service.Kanban
	doc   *openAPIDoc
	board *model.Board
	card  *model.Card
	log   *bytes.Buffer
	user  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	svc := service.New(memory.New(), service.WithClock(func() time.Time { return today }))
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &env{t: t, h: New(svc, logger), svc: svc, doc: spec(t), log: logBuf}
}

// seeded is a board with a label, a card and one comment on it, which is enough
// for every read to have something to return.
func seeded(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.CreateBoard(ctx, "Demo Board", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateLabel(ctx, b.ID, "bug", "#F00"); err != nil {
		t.Fatal(err)
	}
	b, err = e.svc.Board(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.CreateCard(ctx, b.ID, b.Columns[0].ID, service.CardInput{
		Title: "First card", Description: "the description", DueDate: "2026-09-30",
		Assignee: "her@example.com", Labels: []model.ID{b.Labels[0].ID},
		Subtasks: []model.Subtask{{Title: "sub one", Done: true}, {Title: "sub two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.board, e.card, e.user = b, c, "him@example.com"
	return e
}

type response struct {
	*httptest.ResponseRecorder
	body map[string]any
	list []any
}

// do makes the request and holds the answer to the document.
func (e *env) do(method, path, body string, headers ...string) *response {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if e.user != "" {
		req = req.WithContext(identity.NewContext(req.Context(), identity.User{Email: e.user}))
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return e.check(method, path, rr)
}

func (e *env) check(method, path string, rr *httptest.ResponseRecorder) *response {
	e.t.Helper()
	res := &response{ResponseRecorder: rr}
	raw := rr.Body.Bytes()

	// A redirect is the mux's own and not part of the contract. What it has to
	// have is somewhere to go; whether the mux put its usual scrap of HTML in
	// the body is not something a client reads.
	if rr.Code >= 300 && rr.Code < 400 {
		if rr.Header().Get("Location") == "" {
			e.t.Errorf("%s %s: %d without a Location", method, path, rr.Code)
		}
		return res
	}

	schema, hasBody, found := e.doc.responseSchema(method, path, rr.Code)
	if !found {
		// No path or no operation for this method, which is the miss handler
		// answering. Both of those are the shared error body.
		if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			e.t.Errorf("%s %s: answered %d and openapi.json does not describe that",
				method, path, rr.Code)
		}
		schema, hasBody = e.doc.ref("#/components/schemas/Error"), true
	}

	if !hasBody {
		if len(raw) > 0 {
			e.t.Errorf("%s %s: the document gives %d no body and the handler wrote %s",
				method, path, rr.Code, raw)
		}
		return res
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		e.t.Errorf("%s %s: Content-Type is %q", method, path, ct)
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		e.t.Fatalf("%s %s: %d is not JSON: %v: %s", method, path, rr.Code, err, raw)
	}
	for _, p := range e.doc.validate(schema, v, method+" "+path) {
		e.t.Errorf("%s (status %d)", p, rr.Code)
	}
	switch v := v.(type) {
	case map[string]any:
		res.body = v
	case []any:
		res.list = v
	}
	return res
}

// want fails unless the status is the one expected. It reads better at the call
// site than four lines of if, and every call goes through do first, so the
// document has already been consulted by the time this runs.
func (r *response) want(t *testing.T, status int) *response {
	t.Helper()
	if r.Code != status {
		t.Fatalf("got %d, want %d: %s", r.Code, status, r.Body.String())
	}
	return r
}

func (r *response) str(t *testing.T, key string) string {
	t.Helper()
	s, ok := r.body[key].(string)
	if !ok {
		t.Fatalf("%q is not a string in %v", key, r.body)
	}
	return s
}

// --- meta ---------------------------------------------------------------------

func TestIndexAndSpec(t *testing.T) {
	e := newEnv(t)

	res := e.do("GET", "/api/v1/", "").want(t, 200)
	if got := res.str(t, "spec"); got != "/api/v1/openapi.json" {
		t.Errorf("index points at %q", got)
	}

	// The document the handler serves has to be the document, byte for byte:
	// this is the one response whose schema is only "an object".
	spec := e.do("GET", "/api/v1/openapi.json", "").want(t, 200)
	if spec.body["openapi"] != "3.1.0" {
		t.Errorf("openapi is %v", spec.body["openapi"])
	}
	if !bytes.Equal(spec.Body.Bytes(), specJSON) {
		t.Error("the served document is not the embedded one")
	}
}

func TestUnknownEndpointAndMethod(t *testing.T) {
	e := newEnv(t)

	res := e.do("GET", "/api/v1/nope", "").want(t, 404)
	if !strings.Contains(res.str(t, "error"), "openapi.json") {
		t.Errorf("the 404 does not say where the endpoints are: %q", res.body["error"])
	}

	// A path that exists for other methods. The mux decides which of the two
	// this is, so its Allow header survives the body being replaced.
	res = e.do("PATCH", "/api/v1/cards/whatever", "{}").want(t, 405)
	if allow := res.Header().Get("Allow"); !strings.Contains(allow, "PUT") {
		t.Errorf("Allow is %q", allow)
	}

	// /api/v1 without the slash is the mux's own redirect, not a 404. It is a
	// 307, so the method survives it and a POST that lands here is still a POST
	// when it arrives.
	res = e.do("GET", "/api/v1", "").want(t, 307)
	if loc := res.Header().Get("Location"); loc != "/api/v1/" {
		t.Errorf("Location is %q", loc)
	}

	// A doubled slash the mux cleans before it decides. That redirect it hands
	// to the miss handler rather than serving itself, so the status has to
	// arrive with the Location and without a 404 in its place.
	res = e.do("GET", "/api/v1//nope", "").want(t, 307)
	if loc := res.Header().Get("Location"); loc != "/api/v1/nope" {
		t.Errorf("Location is %q", loc)
	}
	if res.Body.Len() != 0 {
		t.Errorf("the cleaned-path redirect has a body: %s", res.Body)
	}
}

// --- boards -------------------------------------------------------------------

func TestBoardLifecycle(t *testing.T) {
	e := newEnv(t)

	if list := e.do("GET", "/api/v1/boards", "").want(t, 200).list; len(list) != 0 {
		t.Errorf("a fresh store has %d boards", len(list))
	}

	created := e.do("POST", "/api/v1/boards",
		`{"name":"Work","columns":["Todo","Doing"]}`).want(t, 201)
	if loc := created.Header().Get("Location"); loc != "/api/v1/boards/work" {
		t.Errorf("Location is %q", loc)
	}
	if got := created.str(t, "slug"); got != "work" {
		t.Errorf("slug is %q, want it derived from the name", got)
	}
	if cols, _ := created.body["columns"].([]any); len(cols) != 2 {
		t.Errorf("created %d columns, want the two that were asked for", len(cols))
	}
	if created.str(t, "layout") != model.LayoutColumns {
		t.Errorf("layout is %q", created.body["layout"])
	}

	e.do("GET", "/api/v1/boards/work", "").want(t, 200)
	e.do("GET", "/api/v1/boards/nope", "").want(t, 404)

	// The slug is taken, and that is a 409 rather than a 400: the request is
	// well formed and the world is in the way.
	e.do("POST", "/api/v1/boards", `{"name":"Work"}`).want(t, 409)

	renamed := e.do("PATCH", "/api/v1/boards/work", `{"name":"Werk"}`).want(t, 200)
	if renamed.str(t, "name") != "Werk" || renamed.str(t, "slug") != "work" {
		t.Errorf("a patch with one field changed the other: %v", renamed.body)
	}

	e.do("PUT", "/api/v1/boards/work/layout", `{"layout":"rows"}`).want(t, 204)
	if got := e.do("GET", "/api/v1/boards/work", "").want(t, 200).str(t, "layout"); got != "rows" {
		t.Errorf("layout is %q after setting rows", got)
	}
	e.do("PUT", "/api/v1/boards/work/layout", `{"layout":"sideways"}`).want(t, 400)

	e.do("DELETE", "/api/v1/boards/work", "").want(t, 204)
	e.do("DELETE", "/api/v1/boards/work", "").want(t, 404)
}

func TestABadRequestNamesTheField(t *testing.T) {
	e := newEnv(t)

	res := e.do("POST", "/api/v1/boards", `{"name":""}`).want(t, 400)
	if res.body["field"] != "name" {
		t.Errorf("field is %v, want name", res.body["field"])
	}

	// A misspelled field is the whole reason unknown fields are refused. The
	// spelling is deliberate, and written this way so that the spell checker in
	// the linter does not read it as one of mine.
	const typo = "na" + "em"
	res = e.do("POST", "/api/v1/boards", `{"`+typo+`":"Work"}`).want(t, 400)
	if !strings.Contains(res.str(t, "error"), typo) {
		t.Errorf("the 400 does not name the unknown field: %q", res.body["error"])
	}

	e.do("POST", "/api/v1/boards", "").want(t, 400)
	e.do("POST", "/api/v1/boards", "{", "Content-Type", "application/json").want(t, 400)
	e.do("POST", "/api/v1/boards", `{"name":"Work"}`, "Content-Type", "text/plain").want(t, 415)

	// A charset on the media type is still JSON.
	e.do("POST", "/api/v1/boards", `{"name":"Charset"}`,
		"Content-Type", "application/json; charset=utf-8").want(t, 201)
}

func TestColumns(t *testing.T) {
	e := seeded(t)

	created := e.do("POST", "/api/v1/boards/demo/columns", `{"name":"Review","wip_limit":2}`).want(t, 201)
	review := created.str(t, "id")
	if created.body["position"] != float64(4) {
		t.Errorf("the new column is at position %v, want the end", created.body["position"])
	}

	e.do("PATCH", "/api/v1/boards/demo/columns/"+review, `{"name":"Peer review"}`).want(t, 204)
	e.do("PATCH", "/api/v1/boards/demo/columns/"+review, `{"name":""}`).want(t, 400)

	// An id that exists on another board is a 404 here, because the service
	// method takes a bare id and would otherwise write to it.
	other, err := e.svc.CreateBoard(context.Background(), "Other", "other", []string{"Only"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := string(other.Columns[0].ID)
	e.do("PATCH", "/api/v1/boards/demo/columns/"+foreign, `{"name":"Mine now"}`).want(t, 404)
	e.do("DELETE", "/api/v1/boards/demo/columns/"+foreign, "").want(t, 404)
	if got, err := e.svc.Board(context.Background(), "other"); err != nil || got.Columns[0].Name != "Only" {
		t.Errorf("the other board's column changed: %v %v", got, err)
	}

	// Every column of the board, exactly once. A partial list is a 400: a board
	// with two columns in third place is not a board anybody can draw.
	e.do("PUT", "/api/v1/boards/demo/columns/order",
		`{"order":["`+review+`","`+string(e.board.Columns[0].ID)+`"]}`).want(t, 400)

	order := `{"order":["` + review + `"`
	for _, c := range e.board.Columns {
		order += `,"` + string(c.ID) + `"`
	}
	e.do("PUT", "/api/v1/boards/demo/columns/order", order+"]}").want(t, 204)
	b := e.do("GET", "/api/v1/boards/demo", "").want(t, 200)
	cols, _ := b.body["columns"].([]any)
	first, _ := cols[0].(map[string]any)
	if first["id"] != review {
		t.Errorf("the order did not take: %v", cols)
	}

	// The cards go with the column unless somebody says where to put them.
	e.do("DELETE", "/api/v1/boards/demo/columns/"+string(e.board.Columns[0].ID)+
		"?move_to="+review, "").want(t, 204)
	card := e.do("GET", "/api/v1/cards/"+string(e.card.ID), "").want(t, 200)
	if card.str(t, "column_id") != review {
		t.Errorf("the card did not move: %v", card.body)
	}
}

func TestLabels(t *testing.T) {
	e := seeded(t)

	created := e.do("POST", "/api/v1/boards/demo/labels", `{"name":"urgent","color":"#3B82F6"}`).want(t, 201)
	if got := created.str(t, "color"); got != "#3b82f6" {
		t.Errorf("colour is %q, want it lower case", got)
	}
	id := created.str(t, "id")

	e.do("PATCH", "/api/v1/boards/demo/labels/"+id, `{"name":"later","color":""}`).want(t, 204)
	e.do("PATCH", "/api/v1/boards/demo/labels/"+id, `{"name":"later","color":"red"}`).want(t, 400)

	other, err := e.svc.CreateBoard(context.Background(), "Other", "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := e.svc.CreateLabel(context.Background(), other.ID, "theirs", "")
	if err != nil {
		t.Fatal(err)
	}
	e.do("PATCH", "/api/v1/boards/demo/labels/"+string(foreign.ID), `{"name":"mine"}`).want(t, 404)
	e.do("DELETE", "/api/v1/boards/demo/labels/"+string(foreign.ID), "").want(t, 404)

	// Deleting a label takes it off the cards that carry it.
	e.do("DELETE", "/api/v1/boards/demo/labels/"+string(e.board.Labels[0].ID), "").want(t, 204)
	card := e.do("GET", "/api/v1/cards/"+string(e.card.ID), "").want(t, 200)
	if labels, _ := card.body["labels"].([]any); len(labels) != 0 {
		t.Errorf("the card still carries %v", labels)
	}
}

// --- cards --------------------------------------------------------------------

func TestCards(t *testing.T) {
	e := seeded(t)

	list := e.do("GET", "/api/v1/boards/demo/cards", "").want(t, 200).list
	if len(list) != 1 {
		t.Fatalf("the seeded board has %d cards", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["due_date"] != "2026-09-30" {
		t.Errorf("due_date is %v, want a plain day", first["due_date"])
	}
	if first["archived_at"] != nil {
		t.Errorf("archived_at is %v on a card that is on the board", first["archived_at"])
	}
	if subtasks, _ := first["subtasks"].([]any); len(subtasks) != 2 {
		t.Errorf("the card has %d subtasks", len(subtasks))
	}

	// No column_id, so the card goes to the first column.
	created := e.do("POST", "/api/v1/boards/demo/cards", `{"title":"Filed by a script"}`).want(t, 201)
	if created.str(t, "column_id") != string(e.board.Columns[0].ID) {
		t.Errorf("column_id is %v, want the first column", created.body["column_id"])
	}
	if loc := created.Header().Get("Location"); loc != "/api/v1/cards/"+created.str(t, "id") {
		t.Errorf("Location is %q", loc)
	}

	e.do("POST", "/api/v1/boards/demo/cards", `{"title":""}`).want(t, 400)
	e.do("POST", "/api/v1/boards/demo/cards", `{"title":"x","due_date":"30.09.2026"}`).want(t, 400)
	e.do("POST", "/api/v1/boards/nope/cards", `{"title":"x"}`).want(t, 404)
	e.do("GET", "/api/v1/cards/nope", "").want(t, 404)
}

func TestUpdateCardMovesItFirst(t *testing.T) {
	e := seeded(t)
	id := string(e.card.ID)
	second := string(e.board.Columns[1].ID)

	// A PUT is the whole card: the description goes because it was left out.
	updated := e.do("PUT", "/api/v1/cards/"+id,
		`{"title":"Renamed","column_id":"`+second+`"}`).want(t, 200)
	if updated.str(t, "title") != "Renamed" || updated.str(t, "description") != "" {
		t.Errorf("the update did not replace the card: %v", updated.body)
	}
	if updated.str(t, "column_id") != second {
		t.Errorf("the card is in %v, want it moved", updated.body["column_id"])
	}
	if subtasks, _ := updated.body["subtasks"].([]any); len(subtasks) != 0 {
		t.Errorf("the subtasks survived a PUT that left them out: %v", subtasks)
	}

	// A subtask sent back with its id keeps it, so ticking one off does not
	// replace the row.
	with := e.do("PUT", "/api/v1/cards/"+id,
		`{"title":"Renamed","subtasks":[{"title":"one"},{"title":"two","done":true}]}`).want(t, 200)
	subtasks, _ := with.body["subtasks"].([]any)
	if len(subtasks) != 2 {
		t.Fatalf("got %d subtasks", len(subtasks))
	}
	kept, _ := subtasks[0].(map[string]any)
	again := e.do("PUT", "/api/v1/cards/"+id,
		`{"title":"Renamed","subtasks":[{"id":"`+kept["id"].(string)+`","title":"one","done":true}]}`).want(t, 200)
	after, _ := again.body["subtasks"].([]any)
	still, _ := after[0].(map[string]any)
	if still["id"] != kept["id"] || still["done"] != true {
		t.Errorf("the subtask was replaced rather than ticked off: %v", after)
	}
}

func TestAWIPLimitRefusesTheMoveAndKeepsTheCard(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	full, err := e.svc.AddColumn(ctx, e.board.ID, "Full", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateCard(ctx, e.board.ID, full.ID, service.CardInput{Title: "Already there"}); err != nil {
		t.Fatal(err)
	}

	id := string(e.card.ID)
	e.do("PUT", "/api/v1/cards/"+id, `{"title":"Moved","column_id":"`+string(full.ID)+`"}`).want(t, 409)

	// The move is refused first, so the title is not written either.
	card := e.do("GET", "/api/v1/cards/"+id, "").want(t, 200)
	if card.str(t, "title") != "First card" || card.str(t, "column_id") != string(e.board.Columns[0].ID) {
		t.Errorf("the refused move left the card changed: %v", card.body)
	}

	// The same limit on the reorder route.
	e.do("PUT", "/api/v1/boards/demo/columns/"+string(full.ID)+"/cards/order",
		`{"order":["`+id+`"]}`).want(t, 409)

	// And a card creation into it.
	e.do("POST", "/api/v1/boards/demo/cards",
		`{"title":"One too many","column_id":"`+string(full.ID)+`"}`).want(t, 409)
}

func TestReorderCardsMovesAcrossColumns(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	second, err := e.svc.CreateCard(ctx, e.board.ID, e.board.Columns[0].ID,
		service.CardInput{Title: "Second card"})
	if err != nil {
		t.Fatal(err)
	}

	// One card named for the second column is a move, which is the drag and
	// drop the page does.
	e.do("PUT", "/api/v1/boards/demo/columns/"+string(e.board.Columns[1].ID)+"/cards/order",
		`{"order":["`+string(second.ID)+`"]}`).want(t, 204)
	card := e.do("GET", "/api/v1/cards/"+string(second.ID), "").want(t, 200)
	if card.str(t, "column_id") != string(e.board.Columns[1].ID) {
		t.Errorf("the card is in %v", card.body["column_id"])
	}

	e.do("PUT", "/api/v1/boards/demo/columns/nope/cards/order", `{"order":[]}`).want(t, 404)
}

func TestAssignee(t *testing.T) {
	e := seeded(t)
	id := string(e.card.ID)

	res := e.do("PUT", "/api/v1/cards/"+id+"/assignee", `{"assignee":"@me"}`).want(t, 200)
	if got := res.str(t, "assignee"); got != e.user {
		t.Errorf("@me resolved to %q, want %q", got, e.user)
	}

	e.do("PUT", "/api/v1/cards/"+id+"/assignee", `{"assignee":""}`).want(t, 200)
	if got := e.do("GET", "/api/v1/cards/"+id, "").want(t, 200).str(t, "assignee"); got != "" {
		t.Errorf("assignee is %q after being cleared", got)
	}

	// Nobody signed in, so @me names nobody and that is the caller's fault
	// rather than a 500.
	e.user = ""
	e.do("PUT", "/api/v1/cards/"+id+"/assignee", `{"assignee":"@me"}`).want(t, 403)
	e.do("PUT", "/api/v1/cards/"+id+"/assignee", `{"assignee":"her@example.com"}`).want(t, 200)
}

func TestToggleLabel(t *testing.T) {
	e := seeded(t)
	id, label := string(e.card.ID), string(e.board.Labels[0].ID)

	off := e.do("POST", "/api/v1/cards/"+id+"/labels/"+label+"/toggle", "").want(t, 200)
	if labels, _ := off.body["labels"].([]any); len(labels) != 0 {
		t.Errorf("the label is still on the card: %v", labels)
	}
	on := e.do("POST", "/api/v1/cards/"+id+"/labels/"+label+"/toggle", "").want(t, 200)
	if labels, _ := on.body["labels"].([]any); len(labels) != 1 {
		t.Errorf("the label did not come back: %v", labels)
	}

	// A label from another board is a 404 and not a card carrying a label its
	// board has never heard of.
	other, err := e.svc.CreateBoard(context.Background(), "Other", "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := e.svc.CreateLabel(context.Background(), other.ID, "theirs", "")
	if err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/api/v1/cards/"+id+"/labels/"+string(foreign.ID)+"/toggle", "").want(t, 404)
}

func TestArchiveAndRestore(t *testing.T) {
	e := seeded(t)
	id := string(e.card.ID)

	e.do("POST", "/api/v1/cards/"+id+"/archive", "").want(t, 204)
	if list := e.do("GET", "/api/v1/boards/demo/cards", "").want(t, 200).list; len(list) != 0 {
		t.Errorf("an archived card is still on the board: %v", list)
	}
	archived := e.do("GET", "/api/v1/boards/demo/archive", "").want(t, 200).list
	if len(archived) != 1 {
		t.Fatalf("the archive holds %d cards", len(archived))
	}
	if card, _ := archived[0].(map[string]any); card["archived_at"] == nil {
		t.Errorf("archived_at is null on an archived card: %v", card)
	}

	e.do("POST", "/api/v1/cards/"+id+"/restore", "").want(t, 204)
	if list := e.do("GET", "/api/v1/boards/demo/cards", "").want(t, 200).list; len(list) != 1 {
		t.Errorf("the restored card is not on the board: %v", list)
	}

	e.do("DELETE", "/api/v1/cards/"+id, "").want(t, 204)
	e.do("DELETE", "/api/v1/cards/"+id, "").want(t, 404)
}

func TestSearchIsTheSameQueryThePageRuns(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	if _, err := e.svc.CreateCard(ctx, e.board.ID, e.board.Columns[0].ID,
		service.CardInput{Title: "Something else", Assignee: e.user}); err != nil {
		t.Fatal(err)
	}

	hits := e.do("GET", "/api/v1/boards/demo/cards?q=first", "").want(t, 200).list
	if len(hits) != 1 {
		t.Errorf("a text search matched %d cards", len(hits))
	}
	hits = e.do("GET", "/api/v1/boards/demo/cards?q=label%3Abug", "").want(t, 200).list
	if len(hits) != 1 {
		t.Errorf("label:bug matched %d cards", len(hits))
	}
	hits = e.do("GET", "/api/v1/boards/demo/cards?q=assignee%3Anobody%40example.com", "").want(t, 200).list
	if len(hits) != 0 {
		t.Errorf("a search for nobody matched %d cards", len(hits))
	}
}

// --- bulk ---------------------------------------------------------------------

func TestBulk(t *testing.T) {
	e := seeded(t)
	ctx := context.Background()
	second, err := e.svc.CreateCard(ctx, e.board.ID, e.board.Columns[0].ID,
		service.CardInput{Title: "Second card"})
	if err != nil {
		t.Fatal(err)
	}
	ids := `"` + string(e.card.ID) + `","` + string(second.ID) + `"`

	moved := e.do("POST", "/api/v1/boards/demo/cards/bulk",
		`{"action":"move","ids":[`+ids+`],"target":"`+string(e.board.Columns[1].ID)+`"}`).want(t, 200)
	if changed, _ := moved.body["changed"].([]any); len(changed) != 2 {
		t.Errorf("moved %v", moved.body)
	}

	assigned := e.do("POST", "/api/v1/boards/demo/cards/bulk",
		`{"action":"assign","ids":[`+ids+`],"target":"@me"}`).want(t, 200)
	if changed, _ := assigned.body["changed"].([]any); len(changed) != 2 {
		t.Errorf("assigned %v", assigned.body)
	}
	card := e.do("GET", "/api/v1/cards/"+string(e.card.ID), "").want(t, 200)
	if card.str(t, "assignee") != e.user {
		t.Errorf("@me in a bulk assign resolved to %q", card.body["assignee"])
	}

	// A card that is not there is one entry in failed, not a failed request.
	partial := e.do("POST", "/api/v1/boards/demo/cards/bulk",
		`{"action":"archive","ids":["`+string(e.card.ID)+`","ghost"]}`).want(t, 200)
	changed, _ := partial.body["changed"].([]any)
	failed, _ := partial.body["failed"].(map[string]any)
	if len(changed) != 1 || len(failed) != 1 || failed["ghost"] != "not found" {
		t.Errorf("the partial failure is not reported: %v", partial.body)
	}
	if !strings.Contains(e.log.String(), "bulk action partially failed") {
		t.Error("a partial failure was not logged")
	}

	e.do("POST", "/api/v1/boards/demo/cards/bulk",
		`{"action":"delete","ids":["`+string(second.ID)+`"]}`).want(t, 200)
	e.do("GET", "/api/v1/cards/"+string(second.ID), "").want(t, 404)

	e.do("POST", "/api/v1/boards/demo/cards/bulk", `{"action":"sing","ids":[`+ids+`]}`).want(t, 400)
	e.do("POST", "/api/v1/boards/demo/cards/bulk", `{"action":"archive","ids":[]}`).want(t, 400)

	e.user = ""
	e.do("POST", "/api/v1/boards/demo/cards/bulk",
		`{"action":"assign","ids":[`+ids+`],"target":"@me"}`).want(t, 403)
}

// --- comments -----------------------------------------------------------------

func TestComments(t *testing.T) {
	e := seeded(t)
	id := string(e.card.ID)

	if list := e.do("GET", "/api/v1/cards/"+id+"/comments", "").want(t, 200).list; len(list) != 0 {
		t.Errorf("a fresh card has %d comments", len(list))
	}
	// A thread asked for on an id that does not exist is a 404, not an empty
	// list: an empty list means a card nobody has written on.
	e.do("GET", "/api/v1/cards/ghost/comments", "").want(t, 404)

	created := e.do("POST", "/api/v1/cards/"+id+"/comments", `{"body":"Looks done to me"}`).want(t, 201)
	if got := created.str(t, "author"); got != e.user {
		t.Errorf("author is %q, want the caller", got)
	}
	comment := created.str(t, "id")

	// There is no author field to send, so one sent is an unknown field.
	e.do("POST", "/api/v1/cards/"+id+"/comments",
		`{"body":"Not me","author":"her@example.com"}`).want(t, 400)
	e.do("POST", "/api/v1/cards/"+id+"/comments", `{"body":"  "}`).want(t, 400)

	// Somebody else cannot remove it.
	e.user = "her@example.com"
	e.do("DELETE", "/api/v1/comments/"+comment, "").want(t, 403)

	e.user = "him@example.com"
	e.do("DELETE", "/api/v1/comments/"+comment, "").want(t, 204)
	e.do("DELETE", "/api/v1/comments/"+comment, "").want(t, 404)
}

// TestNoIdentityIsOneUser is the deployment with no authentication at all,
// where the author is the empty address and so is the caller.
func TestNoIdentityIsOneUser(t *testing.T) {
	e := seeded(t)
	e.user = ""
	id := string(e.card.ID)

	created := e.do("POST", "/api/v1/cards/"+id+"/comments", `{"body":"Anybody"}`).want(t, 201)
	if got := created.str(t, "author"); got != "" {
		t.Errorf("author is %q where there is no identity", got)
	}
	e.do("DELETE", "/api/v1/comments/"+created.str(t, "id"), "").want(t, 204)
}

// TestNewWithoutALoggerDoesNotPanic covers the nil the mount in cmd/kanban
// could pass on a bad day.
func TestNewWithoutALoggerDoesNotPanic(t *testing.T) {
	h := New(service.New(memory.New()), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("got %d", rr.Code)
	}
}

// TestBoardSLA is the response-time promise over the API. The board's own
// representation carries it, so a client that reads a board sees the promise it
// is being held to without a second request.
func TestBoardSLA(t *testing.T) {
	e := seeded(t)

	// A board with no promise still reports the shape, so a client does not have
	// to tell "off" from "absent".
	board := e.do("GET", "/api/v1/boards/demo", "").want(t, 200)
	sla, ok := board.body["sla"].(map[string]any)
	if !ok {
		t.Fatalf("a board without a promise has no sla object: %v", board.body)
	}
	if sla["response_hours"] != float64(0) {
		t.Errorf("response_hours is %v on a board with no promise", sla["response_hours"])
	}

	e.do("PUT", "/api/v1/boards/demo/sla",
		`{"response_hours":4,"days":["mon","tue","wed","thu","fri"],"start":"08:00","end":"17:00","zone":"Europe/Zurich"}`,
	).want(t, 204)

	sla = e.do("GET", "/api/v1/boards/demo", "").want(t, 200).body["sla"].(map[string]any)
	for field, want := range map[string]any{
		"response_hours": float64(4), "start": "08:00", "end": "17:00", "zone": "Europe/Zurich",
	} {
		if sla[field] != want {
			t.Errorf("%s is %v, want %v", field, sla[field], want)
		}
	}
	if days, _ := sla["days"].([]any); len(days) != 5 || days[0] != "mon" || days[4] != "fri" {
		t.Errorf("days is %v, want the office week", sla["days"])
	}

	// A desk staffed around the clock. A time of day cannot say 24:00, so the
	// end of the day goes back as the midnight it is.
	e.do("PUT", "/api/v1/boards/demo/sla",
		`{"response_hours":2,"days":["mon","tue","wed","thu","fri","sat","sun"],"start":"00:00","end":"00:00"}`,
	).want(t, 204)
	sla = e.do("GET", "/api/v1/boards/demo", "").want(t, 200).body["sla"].(map[string]any)
	if sla["start"] != "00:00" || sla["end"] != "00:00" || len(sla["days"].([]any)) != 7 {
		t.Errorf("sla is %v, want the whole week from midnight to midnight", sla)
	}

	// A PUT is the whole promise, and what it leaves out is the office week the
	// document names rather than whatever the board held before. Which is how
	// the clock is switched off in one line.
	e.do("PUT", "/api/v1/boards/demo/sla", `{"response_hours":0}`).want(t, 204)
	sla = e.do("GET", "/api/v1/boards/demo", "").want(t, 200).body["sla"].(map[string]any)
	if sla["response_hours"] != float64(0) || sla["start"] != "08:00" || sla["end"] != "17:00" {
		t.Errorf("sla is %v after a promise that named only its hours", sla)
	}
	if days, _ := sla["days"].([]any); len(days) != 5 {
		t.Errorf("days is %v, want the office week the document names", sla["days"])
	}

	for _, body := range []string{
		`{"response_hours":-1}`,
		`{"response_hours":100000}`,
		`{"response_hours":4,"days":["sunnday"]}`,
		`{"response_hours":4,"days":[]}`,
		`{"response_hours":4,"start":"half eight"}`,
		`{"response_hours":4,"start":"25:00"}`,
		`{"response_hours":4,"start":"17:00","end":"08:00"}`,
		`{"response_hours":4,"zone":"Mars/Olympus"}`,
	} {
		res := e.do("PUT", "/api/v1/boards/demo/sla", body).want(t, 400)
		if res.body["field"] == nil {
			t.Errorf("the 400 for %s names no field: %v", body, res.body)
		}
	}

	e.do("PUT", "/api/v1/boards/demo/sla", `{"resposne_hours":4}`).want(t, 400)
	e.do("PUT", "/api/v1/boards/demo/sla", `{"response_hours":4}`, "Content-Type", "text/plain").want(t, 415)
	e.do("PUT", "/api/v1/boards/nope/sla", `{"response_hours":4}`).want(t, 404)
}

// TestColumnStopsClock covers the column flag the response-time clock reads, over
// the API where it is a field of its own rather than a checkbox.
func TestColumnStopsClock(t *testing.T) {
	e := seeded(t)

	created := e.do("POST", "/api/v1/boards/demo/columns", `{"name":"Waiting","stops_clock":true}`).want(t, 201)
	if created.body["stops_clock"] != true {
		t.Fatalf("stops_clock is %v on the column that asked for it", created.body["stops_clock"])
	}
	id := created.str(t, "id")

	// A patch carries the whole column, so it has to say so again to keep it.
	e.do("PATCH", "/api/v1/boards/demo/columns/"+id, `{"name":"Waiting","stops_clock":true}`).want(t, 204)
	board := e.do("GET", "/api/v1/boards/demo", "").want(t, 200)
	for _, c := range board.body["columns"].([]any) {
		col := c.(map[string]any)
		if col["id"] == id && col["stops_clock"] != true {
			t.Errorf("stops_clock is %v after the patch", col["stops_clock"])
		}
		// Every other column says so rather than leaving the field out.
		if col["id"] != id && col["stops_clock"] != false {
			t.Errorf("column %v has stops_clock %v, want false", col["name"], col["stops_clock"])
		}
	}

	// And a patch that leaves it out takes it off, the same as it does the WIP
	// limit, because on this route the fields it does not carry are cleared.
	e.do("PATCH", "/api/v1/boards/demo/columns/"+id, `{"name":"Waiting"}`).want(t, 204)
	board = e.do("GET", "/api/v1/boards/demo", "").want(t, 200)
	for _, c := range board.body["columns"].([]any) {
		if col := c.(map[string]any); col["id"] == id && col["stops_clock"] != false {
			t.Errorf("stops_clock is %v after a patch that left it out, want false", col["stops_clock"])
		}
	}
}
