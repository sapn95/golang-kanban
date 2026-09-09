package assets

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The compile is only as good as its content globs: a class in a file Tailwind
// never reads produces no rule, and the element is simply unstyled on the page.
// CI regenerates the stylesheet and fails on a diff, which catches a class added
// to a file that is already listed. These tests catch the other half, a file that
// writes classes and is not listed at all.

// contentGlobs is the content array of tailwind.config.js, read as text. The
// config is JavaScript and nothing here runs JavaScript, which is the point: the
// list is short, and a test that parses it is cheaper than a Node dependency.
func contentGlobs(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("tailwind.config.js")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(src), "content: [")
	if !ok {
		t.Fatal("tailwind.config.js has no `content: [` array")
	}
	list, _, ok := strings.Cut(rest, "]")
	if !ok {
		t.Fatal("tailwind.config.js: the content array does not close")
	}
	var globs []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(list, -1) {
		globs = append(globs, m[1])
	}
	if len(globs) == 0 {
		t.Fatal("tailwind.config.js lists no content globs, so nothing compiles")
	}
	return globs
}

// covers is the subset of glob syntax the config uses: a literal path, or one
// `**/` standing for any number of directories.
func covers(glob, file string) bool {
	glob = strings.TrimPrefix(glob, "./")
	if prefix, pattern, found := strings.Cut(glob, "**/"); found {
		if !strings.HasPrefix(file, prefix) {
			return false
		}
		ok, _ := path.Match(pattern, path.Base(file))
		return ok
	}
	ok, _ := path.Match(glob, file)
	return ok
}

// writesClasses is anything that can put a class on an element: the attribute in
// markup, and the two ways JavaScript sets one.
var writesClasses = regexp.MustCompile(`class="|classList|className`)

func TestEveryFileThatWritesAClassIsCompiled(t *testing.T) {
	globs := contentGlobs(t)
	matched := make(map[string]int, len(globs))

	root := ".." // the module root; this package is assets/
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			// Third-party code brings its own styling and is not scanned;
			// .tools holds the downloaded compiler.
			case ".git", ".tools", "assets/vendor":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(rel) {
		case ".html", ".js", ".mjs", ".go":
		default:
			return nil
		}
		for _, glob := range globs {
			if covers(glob, rel) {
				matched[glob]++
				return nil
			}
		}
		// A class in a test asserts what a page renders; it does not put one
		// there, so it needs no rule of its own.
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		if writesClasses.Match(body) {
			t.Errorf("%s writes class names and no content glob in "+
				"assets/tailwind.config.js covers it, so its classes compile to nothing", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, glob := range globs {
		if matched[glob] == 0 {
			t.Errorf("the content glob %q matches no file: it was probably left behind by a rename", glob)
		}
	}
}

// The theme is a class on <html>. With Tailwind's default darkMode every dark:
// utility compiles into a prefers-color-scheme query instead, which reads the
// operating system and ignores the toggle — a page that looks right on a dark
// laptop and never changes.
func TestDarkModeIsCompiledAsAClassNotAMediaQuery(t *testing.T) {
	css, err := os.ReadFile("tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ":is(.dark *)") {
		t.Error("tailwind.css has no rule scoped to the dark class; check darkMode in tailwind.config.js")
	}
	if strings.Contains(string(css), "prefers-color-scheme") {
		t.Error("tailwind.css follows the system setting, so the dark-mode toggle only half works")
	}
}
