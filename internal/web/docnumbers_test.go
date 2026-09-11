package web

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"kanban/assets"
)

// A number in prose that nothing checks is a number that goes stale, and this
// repository has just spent a review round finding out how many of them had.
// The tests below hold the ones that are cheap to hold.
//
// They are deliberately not a style rule about writing numbers down: a figure
// is worth stating when it tells somebody something. It is worth checking for
// the same reason.

// TestStylesheetSizeInProse holds the two places that print how big the
// compiled stylesheet is. Both said 38 kB when it was 45.
func TestStylesheetSizeInProse(t *testing.T) {
	css, err := fs.ReadFile(assets.FS(), "tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(css); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	kb := (len(css) + 500) / 1000
	gz := (buf.Len() + 500) / 1000

	for _, tc := range []struct{ file, want string }{
		{"readme.md", fmt.Sprintf("into a %d kB stylesheet", kb)},
		{"docs/adr/0011-tailwind-is-compiled.md", fmt.Sprintf("is the output, %d kB and %d kB gzipped", kb, gz)},
	} {
		body := repoFile(t, tc.file)
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s does not say %q; the stylesheet is %d kB and %d kB gzipped",
				tc.file, tc.want, kb, gz)
		}
	}
}

// TestRouteCountInProse holds the two sentences in docs/api.md that count the
// routes. The table beside them is checked route by route; the prose was not,
// and said 36 when there were 43.
func TestRouteCountInProse(t *testing.T) {
	src := repoFile(t, "internal/web/server.go")
	n := len(regexp.MustCompile(`mux\.Handle(?:Func)?\("`).FindAllString(src, -1))
	if n == 0 {
		t.Fatal("no route registrations found, so this test is reading the wrong file")
	}
	body := repoFile(t, "docs/api.md")
	for _, want := range []string{
		fmt.Sprintf("all %d of them", n),
		fmt.Sprintf("The last of those %d", n),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/api.md does not say %q; server.go registers %d routes", want, n)
		}
	}
	// And no other count of routes left behind by an earlier edit.
	for _, m := range regexp.MustCompile(`all (\d+) of them`).FindAllStringSubmatch(body, -1) {
		if got, _ := strconv.Atoi(m[1]); got != n {
			t.Errorf("docs/api.md still says %d routes somewhere", got)
		}
	}
}

// repoFile returns a file from the repository root, whatever package the test runs
// in.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
