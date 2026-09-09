package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"kanban/internal/api"
)

// The JSON API is mounted on this mux, so the two packages have to agree on
// where. web does not import api outside this test, which is why the prefix is
// written out twice and compared here rather than shared as a constant.
func TestTheMountPointIsWhereTheAPIServes(t *testing.T) {
	if apiPrefix != api.Prefix {
		t.Errorf("web mounts the API at %q and api serves %q", apiPrefix, api.Prefix)
	}
}

// mountedAPI is a seeded board whose handler has the JSON API on it. The option
// needs the service and the env is what creates it, so the handler is built a
// second time rather than passed the option up front.
func mountedAPI(t *testing.T) *env {
	t.Helper()
	e := seeded(t)
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.h = New(e.svc, nil, logger,
		WithClock(func() time.Time { return today }),
		WithAPI(api.New(e.svc, logger)))
	return e
}

func TestTheMountedAPIAnswersAndIsLogged(t *testing.T) {
	e := mountedAPI(t)

	rr := e.do("GET", "/api/v1/boards", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/boards is %d: %s", rr.Code, rr.Body)
	}
	var boards []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &boards); err != nil {
		t.Fatalf("the answer is not a JSON list: %v", err)
	}
	if len(boards) != 1 || boards[0]["slug"] != "demo" {
		t.Errorf("the API sees %v, and the pages see the demo board", boards)
	}
	// The request log line is the proof that this went through the middleware
	// rather than around it.
	if !strings.Contains(e.log.String(), "/api/v1/boards") {
		t.Error("the request was not logged")
	}
}

// The API is behind the same cross-site check as the pages, and answers a
// refusal in its own shape. openapi.json declares an object with an "error"
// field for every failure, and this one is written before the api package sees
// the request.
func TestACrossSiteWriteToTheAPIIsRefusedInJSON(t *testing.T) {
	e := mountedAPI(t)

	rr := e.do("POST", "/api/v1/boards", strings.NewReader(`{"name":"Nope"}`),
		"Content-Type", "application/json", "Sec-Fetch-Site", "cross-site")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type is %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("the refusal says nothing: %v", body)
	}
	if boards, _ := e.svc.Boards(t.Context()); len(boards) != 1 {
		t.Errorf("the refused write created something: %d boards", len(boards))
	}

	// The same refusal on a page path is still one line of text, because it is
	// swapped into a page or shown as it is.
	rr = e.do("POST", "/boards", form("name", "Nope"), "Sec-Fetch-Site", "cross-site")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type is %q", ct)
	}
}

// Without the option there is no route under the prefix at all, and the page
// mux answers it the way it answers any other unknown path.
func TestWithoutTheOptionTheAPIIsNotThere(t *testing.T) {
	e := seeded(t)
	if rr := e.do("GET", "/api/v1/boards", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/boards is %d without WithAPI, want 404", rr.Code)
	}
}
