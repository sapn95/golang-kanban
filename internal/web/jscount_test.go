package web

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"kanban/assets"
)

func TestCountLines(t *testing.T) {
	for _, tc := range []struct {
		name, src   string
		code, total int
	}{
		{"blank lines are not code", "a\n\n\nb\n", 2, 4},
		{"a line comment is not code", "a\n// why\nb\n", 2, 3},
		{"a block comment is not code", "a\n/* why\n   and why\n*/\nb\n", 2, 5},
		{"code beside a block comment counts once", "a /* why */ b\n", 1, 1},
		{"code after a block ends counts", "/* why\n*/ b\n", 1, 2},
		{"a file with no trailing newline keeps its last line", "a\nb", 2, 2},
		{"empty", "", 0, 0},
		{"only comments", "// a\n/* b */\n", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, total := countLines(tc.src)
			if code != tc.code || total != tc.total {
				t.Errorf("code=%d total=%d, want %d and %d", code, total, tc.code, tc.total)
			}
		})
	}
}

// The number on the page has to be the number in the files, or it is decoration.
func TestJSCountMatchesTheFiles(t *testing.T) {
	var wantCode, wantTotal int
	var files []string
	if err := fs.WalkDir(assets.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") || strings.HasPrefix(path, "vendor/") {
			return err
		}
		b, rerr := fs.ReadFile(assets.FS(), path)
		if rerr != nil {
			return rerr
		}
		c, tt := countLines(string(b))
		wantCode, wantTotal = wantCode+c, wantTotal+tt
		files = append(files, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no .js found outside vendor/, so this test is reading the wrong tree")
	}

	// Plus the inline script in the head, which is the one piece of JavaScript
	// that cannot live in a file: it sets the theme before the first paint.
	layout, err := fs.ReadFile(templateFiles, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	var inline int
	for _, m := range inlineScript.FindAllStringSubmatch(string(layout), -1) {
		if strings.Contains(m[1], "src=") {
			continue
		}
		c, tt := countLines(m[2])
		inline += c
		wantCode, wantTotal = wantCode+c, wantTotal+tt
	}
	if inline == 0 {
		t.Error("the inline script in the head counted as nothing; it is JavaScript that runs on every page")
	}

	got := countJS()
	if got.Code != wantCode || got.Total != wantTotal {
		t.Errorf("countJS = %+v, want Code=%d Total=%d (from %v plus the inline script)",
			got, wantCode, wantTotal, files)
	}
	if got.Handlers == 0 {
		t.Error("no inline event handlers found; the templates have onclick and onsubmit attributes")
	}
}

// A vendored library is somebody else's work, shipped minified, so its line
// count measures how it was packaged rather than how much code is here.
func TestVendoredLibrariesAreNotCounted(t *testing.T) {
	var vendored int
	_ = fs.WalkDir(assets.FS(), "vendor", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, _ := fs.ReadFile(assets.FS(), path)
		vendored += len(b)
		return nil
	})
	if vendored == 0 {
		t.Fatal("no vendored JavaScript found, so this test proves nothing")
	}
	// htmx and Sortable are tens of kilobytes. A count that included them
	// could not be this small.
	if got := countJS().Code; got > 1000 {
		t.Errorf("%d lines counted; that is the vendored %d bytes leaking in", got, vendored)
	}
}

func TestFooterPrintsTheCount(t *testing.T) {
	e := seeded(t)
	body := e.do(http.MethodGet, "/b/demo", nil).Body.String()
	if !strings.Contains(body, "lines of JS") {
		t.Fatal("the footer does not print the count")
	}
	for _, want := range []string{"neither blank nor a comment", "vendored and not counted"} {
		if !strings.Contains(body, want) {
			t.Errorf("the tooltip does not say %q, so the number means nothing definite", want)
		}
	}
}
