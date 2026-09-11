package web

import (
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"kanban/assets"
)

// jsCount is how much JavaScript this project wrote, for the footer.
//
// The board is a fork of something whose whole idea is a server-rendered page,
// and the amount of script on it is the thing most likely to drift without
// anybody deciding to let it. A number on every page is a number somebody
// notices going up.
//
// Counted from the embedded trees at runtime rather than written down at build
// time, because a figure in a constant is a figure that is wrong by the second
// commit.
type jsCount struct {
	// Code is lines that are neither blank nor comment: the headline figure.
	Code int
	// Total is every line including blanks and comments, so the headline can
	// be checked against something.
	Total int
	// Handlers is the inline event attributes in the templates, onclick and
	// onsubmit and the four other kinds the pattern below lists. They are
	// JavaScript and they are not lines, so they are their own number rather
	// than folded into one that would then mean nothing.
	Handlers int
}

var countJS = sync.OnceValue(func() jsCount {
	var c jsCount

	// The served .js files, minus the vendored libraries. htmx and SortableJS
	// are somebody else's work and are shipped minified, so counting their
	// lines would measure how they were packaged rather than how much code is
	// here.
	_ = fs.WalkDir(assets.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		if strings.HasPrefix(path, "vendor/") {
			return nil
		}
		b, err := fs.ReadFile(assets.FS(), path)
		if err != nil {
			return nil
		}
		code, total := countLines(string(b))
		c.Code += code
		c.Total += total
		return nil
	})

	// The inline <script> in the head, which sets the theme before the first
	// paint and so cannot live in a deferred file. It runs on every page, so
	// leaving it out would be flattering rather than accurate.
	_ = fs.WalkDir(templateFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		b, err := fs.ReadFile(templateFiles, path)
		if err != nil {
			return nil
		}
		body := string(b)
		for _, m := range inlineScript.FindAllStringSubmatch(body, -1) {
			// A <script src=...> is a file, and it is counted above or it is
			// vendored. Only a block with code in it counts here.
			if strings.Contains(m[1], "src=") {
				continue
			}
			code, total := countLines(m[2])
			c.Code += code
			c.Total += total
		}
		c.Handlers += len(inlineHandler.FindAllString(body, -1))
		return nil
	})
	return c
})

var (
	inlineScript  = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	inlineHandler = regexp.MustCompile(`\son(?:click|submit|change|input|keydown|keyup)="`)
)

// countLines returns the lines of src that carry code, and the lines it has.
//
// A line is not code when it is blank, when it starts a `//` comment, or when
// it falls inside a `/* */` block. Nothing here parses JavaScript: a `//` that
// begins a line is a comment in every file anybody writes, and the rule is
// stated in the footer's tooltip so the number means one definite thing rather
// than approximately something.
func countLines(src string) (code, total int) {
	if src == "" {
		return 0, 0
	}
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		total++
		s := strings.TrimSpace(line)
		if inBlock {
			if i := strings.Index(s, "*/"); i >= 0 {
				inBlock = false
				s = strings.TrimSpace(s[i+2:])
			} else {
				continue
			}
		}
		for {
			i := strings.Index(s, "/*")
			if i < 0 {
				break
			}
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				inBlock = true
				s = strings.TrimSpace(s[:i])
				break
			}
			s = strings.TrimSpace(s[:i] + s[i+2+j+2:])
		}
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		code++
	}
	// A file that does not end in a newline still has its last line, and one
	// that does has an empty one after it that is no line at all.
	if strings.HasSuffix(src, "\n") {
		total--
	}
	return code, total
}
