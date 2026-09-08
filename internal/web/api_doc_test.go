package web

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// The endpoint reference is a hand-written table, and a hand-written table
// nobody checks is wrong within two releases. This reads the routes out of
// server.go and the rows out of docs/api.md and fails on a difference in
// either direction, so a new route has to be written down and a row for a
// route that is gone has to go with it.
//
// It reads the source rather than the mux because http.ServeMux cannot be
// asked what it holds, and because one route is registered only when pictures
// are configured. The source has it either way.
func TestEveryRouteIsInTheEndpointReference(t *testing.T) {
	registered := routesInSource(t, "server.go")
	documented := routesInDoc(t, filepath.Join("..", "..", "docs", "api.md"))

	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("%s is registered and not in docs/api.md", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("docs/api.md has a row for %s, which is not registered in server.go", d)
		}
	}
	// Membership alone would let one route answer for two rows, which is how a
	// route gets a second description that says something else.
	for i := 1; i < len(documented); i++ {
		if documented[i] == documented[i-1] {
			t.Errorf("docs/api.md has more than one row for %s", documented[i])
		}
	}
}

var (
	// mux.HandleFunc("GET /cards/{id}", ...) and the one mux.Handle for
	// /assets/. Anchored at the start of a statement, so a registration that
	// is commented out is not a route.
	sourceRoute = regexp.MustCompile(`(?m)^\t*mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)"`)
	// The same call without its pattern, which is how a registration whose
	// pattern is not a plain string literal is still counted.
	registerCall = regexp.MustCompile(`(?m)^\t*mux\.Handle(?:Func)?\(`)
	// | `GET /cards/{id}` | ... , the first cell of a table row.
	docRoute = regexp.MustCompile("(?m)^\\|\\s*`([A-Z]+ /[^`]*)`")
)

// routesInSource is the one side that can fail quietly: a registration the
// regexp does not match is a route this test then never looks for, and if the
// document is missing it too, both comparisons above pass. So count the calls
// as well as the patterns. Every mux.Handle and mux.HandleFunc has to yield
// exactly one route.
func routesInSource(t *testing.T, path string) []string {
	t.Helper()
	src := read(t, path)
	found := patterns(src, sourceRoute)
	if calls := len(registerCall.FindAllString(src, -1)); len(found) != calls {
		t.Fatalf("%s registers %d routes and the pattern found %d of them: %v",
			path, calls, len(found), found)
	}
	slices.Sort(found)
	return found
}

func routesInDoc(t *testing.T, path string) []string {
	t.Helper()
	found := patterns(read(t, path), docRoute)
	slices.Sort(found)
	return found
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func patterns(text string, re *regexp.Regexp) []string {
	var found []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		found = append(found, asWritten(m[1]))
	}
	return found
}

// asWritten turns the two patterns whose Go spelling would be noise in a
// document into the way the document spells them. Everything else is the same
// string on both sides.
func asWritten(pattern string) string {
	switch pattern {
	case "GET /{$}":
		// {$} only says "and nothing after the slash".
		return "GET /"
	case "GET /assets/":
		// A prefix match, and the document gives the tree one row.
		return "GET /assets/{path}"
	}
	return pattern
}
