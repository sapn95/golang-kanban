package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"kanban/internal/service"
)

// The renderer is the one place a string a user typed becomes markup, so these
// tests come in two halves. The first says what the subset does. The second says
// what it refuses, and the fuzz target at the bottom says that nothing outside
// the tag set below ever reaches the page, whatever is typed.

func TestMarkdownRenders(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"a paragraph", "Ordered the part.", "<p>Ordered the part.</p>"},
		{
			"a newline is a break, because the box behaved that way before",
			"Basho Kaminsky\nBrunngasse 58",
			"<p>Basho Kaminsky<br>Brunngasse 58</p>",
		},
		{
			"a blank line is a new paragraph",
			"Waiting on the invoice.\n\nThen it ships.",
			"<p>Waiting on the invoice.</p><p>Then it ships.</p>",
		},
		{"headings start at h4", "# Steps\n## Later", "<h4>Steps</h4><h5>Later</h5>"},
		{"deeper headings all stop at h6", "### a\n#### b\n###### c", "<h6>a</h6><h6>b</h6><h6>c</h6>"},
		{"a hash without a space is not a heading", "#1 on the list", "<p>#1 on the list</p>"},
		{"closing hashes come off", "## Steps ##", "<h5>Steps</h5>"},
		{
			"a bullet list, any of the three markers",
			"- one\n* two\n+ three",
			"<ul><li>one</li><li>two</li><li>three</li></ul>",
		},
		{"a numbered list", "1. one\n2) two", "<ol><li>one</li><li>two</li></ol>"},
		{
			"the two kinds of list do not run together",
			"- one\n1. two",
			"<ul><li>one</li></ul><ol><li>two</li></ol>",
		},
		{
			"a list under a line of prose, without the blank line nobody types",
			"Steps:\n- one\n- two",
			"<p>Steps:</p><ul><li>one</li><li>two</li></ul>",
		},
		{"a dash and a digit are not a list", "-1 degree\n2026 was fine", "<p>-1 degree<br>2026 was fine</p>"},
		{"a quote", "> Not our problem.", "<blockquote><p>Not our problem.</p></blockquote>"},
		{
			"a quote of two lines is one quote",
			"> first\n> second",
			"<blockquote><p>first<br>second</p></blockquote>",
		},
		{"a thematic break", "above\n\n---\n\nbelow", "<p>above</p><hr><p>below</p>"},
		{"bold", "**shipped**", "<p><strong>shipped</strong></p>"},
		{"bold with underscores", "__shipped__", "<p><strong>shipped</strong></p>"},
		{"italic", "*maybe*", "<p><em>maybe</em></p>"},
		{"struck out", "~~was due~~ moved", "<p><del>was due</del> moved</p>"},
		{"nested emphasis", "**very *late***", "<p><strong>very <em>late</em></strong></p>"},
		{"a lone asterisk is an asterisk", "2 * 3 = 6", "<p>2 * 3 = 6</p>"},
		{"an unclosed run is text", "**almost", "<p>**almost</p>"},
		{"snake case survives", "the flag is auto_migrate_on", "<p>the flag is auto_migrate_on</p>"},
		{"an underscore mid-word is not emphasis", "a_b_c", "<p>a_b_c</p>"},
		{"an escape prints the character", `\*not emphasis\*`, "<p>*not emphasis*</p>"},
		{"inline code", "run `kanban migrate` first", "<p>run <code>kanban migrate</code> first</p>"},
		{"code is not markdown", "`**not bold**`", "<p><code>**not bold**</code></p>"},
		{"a doubled fence holds a backtick", "`` a ` b ``", "<p><code>a ` b</code></p>"},
		{
			"a fenced block, language dropped",
			"```sh\ngo build ./cmd/kanban\n```",
			"<pre><code>go build ./cmd/kanban\n</code></pre>",
		},
		{
			"a fence that never closes ends with the description",
			"```\nstill code",
			"<pre><code>still code\n</code></pre>",
		},
		{
			"the other fence is text inside one",
			"```\n~~~\n```",
			"<pre><code>~~~\n</code></pre>",
		},
		{
			"a link",
			"see [the runbook](https://example.com/runbook)",
			`<p>see <a href="https://example.com/runbook" target="_blank" rel="noopener noreferrer nofollow" draggable="false">the runbook</a></p>`,
		},
		{
			"a link with a title",
			`[docs](https://example.com "The docs")`,
			`<p><a href="https://example.com" title="The docs" target="_blank" rel="noopener noreferrer nofollow" draggable="false">docs</a></p>`,
		},
		{
			"a path stays in the tab, because it is this board",
			"[the archive](/b/board/archive)",
			`<p><a href="/b/board/archive" draggable="false">the archive</a></p>`,
		},
		{
			"an address",
			"[write in](mailto:ops@example.com)",
			`<p><a href="mailto:ops@example.com" target="_blank" rel="noopener noreferrer nofollow" draggable="false">write in</a></p>`,
		},
		{
			"a bare URL becomes a link",
			"logs at https://example.com/logs?a=1&b=2 today",
			`<p>logs at <a href="https://example.com/logs?a=1&amp;b=2" target="_blank" rel="noopener noreferrer nofollow" draggable="false">https://example.com/logs?a=1&amp;b=2</a> today</p>`,
		},
		{
			"the full stop after a URL is the sentence's",
			"see https://example.com.",
			`<p>see <a href="https://example.com" target="_blank" rel="noopener noreferrer nofollow" draggable="false">https://example.com</a>.</p>`,
		},
		{
			"a bracket opened inside the URL closes inside it",
			"(https://example.com/a_(b))",
			`<p>(<a href="https://example.com/a_(b)" target="_blank" rel="noopener noreferrer nofollow" draggable="false">https://example.com/a_(b)</a>)</p>`,
		},
		{
			"a URL in a link is the link's",
			"[here](https://example.com)",
			`<p><a href="https://example.com" target="_blank" rel="noopener noreferrer nofollow" draggable="false">here</a></p>`,
		},
		{
			"an image is the link it points at, and nothing is fetched",
			"![a graph](https://example.com/g.png)",
			`<p><a href="https://example.com/g.png" target="_blank" rel="noopener noreferrer nofollow" draggable="false">a graph</a></p>`,
		},
		{"nothing at all", "", ""},
		{"only whitespace", "   \n\t\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(renderMarkdown(c.in)); got != c.want {
				t.Errorf("markdown(%q)\n got: %s\nwant: %s", c.in, got, c.want)
			}
		})
	}
}

// TestMarkdownEscapes is the half of the renderer that matters. A description is
// the longest string on the board a user controls, and it is rendered on a page
// beside everybody else's cards.
func TestMarkdownEscapes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"a tag is text", "<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>"},
		{"a tag in code is text too", "`<img onerror=x>`", "<p><code>&lt;img onerror=x&gt;</code></p>"},
		{"a tag in a fence is text", "```\n<b>no</b>\n```", "<pre><code>&lt;b&gt;no&lt;/b&gt;\n</code></pre>"},
		{"an ampersand", "Q&A", "<p>Q&amp;A</p>"},
		{"an entity is not decoded", "&lt;b&gt;", "<p>&amp;lt;b&amp;gt;</p>"},
		{"a heading is text too", "# <em>x</em>", "<h4>&lt;em&gt;x&lt;/em&gt;</h4>"},
		{
			"javascript: is refused, and the text is still readable",
			"[click](javascript:alert(1))",
			"<p>[click](javascript:alert(1))</p>",
		},
		{
			"and so is data:",
			"[click](data:text/html;base64,PHNjcmlwdD4=)",
			"<p>[click](data:text/html;base64,PHNjcmlwdD4=)</p>",
		},
		{
			"a scheme cannot be smuggled past the check by case",
			"[click](JaVaScRiPt:alert(1))",
			"<p>[click](JaVaScRiPt:alert(1))</p>",
		},
		{
			"a protocol-relative URL is refused: that is another host",
			"[click](//evil.example.com)",
			"<p>[click](//evil.example.com)</p>",
		},
		{
			"a quote in a destination cannot end the attribute",
			`[click](https://example.com/" onmouseover="alert(1))`,
			`<p>[click](https://example.com/&#34; onmouseover=&#34;alert(1))</p>`,
		},
		{
			"nor can one in a title",
			`[click](https://example.com "a\" onmouseover=\"alert(1)")`,
			`<p><a href="https://example.com" title="a\&#34; onmouseover=\&#34;alert(1)" target="_blank" rel="noopener noreferrer nofollow" draggable="false">click</a></p>`,
		},
		{
			"a tag in link text is text",
			"[<b>x</b>](https://example.com)",
			`<p><a href="https://example.com" target="_blank" rel="noopener noreferrer nofollow" draggable="false">&lt;b&gt;x&lt;/b&gt;</a></p>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(renderMarkdown(c.in))
			if got != c.want {
				t.Errorf("markdown(%q)\n got: %s\nwant: %s", c.in, got, c.want)
			}
			// The destination of a refused link is printed as text, so
			// "javascript:" appearing in the output is not the failure. An href
			// that is not one of the four allowed shapes is.
			for _, href := range hrefs(got) {
				if _, _, ok := safeURL(href); !ok {
					t.Errorf("markdown(%q) wrote href=%q: %s", c.in, href, got)
				}
			}
		})
	}
}

var hrefPattern = regexp.MustCompile(`href="([^"]*)"`)

// hrefs is every destination in the output, so a test can hold the renderer to
// what it may link to rather than to the exact markup around it.
func hrefs(html string) []string {
	var out []string
	for _, m := range hrefPattern.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// The tags this renderer is allowed to write, with the attributes each may
// carry. Anything else in the output is a tag the input smuggled through, which
// is what the fuzz target below looks for.
var allowedTag = regexp.MustCompile(`^</?(?:p|br|hr|h4|h5|h6|ul|ol|li|pre|code|blockquote|strong|em|del)>$|` +
	`^<a href="[^"<>]*"(?: title="[^"<>]*")?(?: target="_blank" rel="noopener noreferrer nofollow")? draggable="false">$|^</a>$`)

// FuzzMarkdown holds the invariant the type of the return value promises. It
// returns template.HTML, which tells html/template not to escape it, so the
// renderer is the escaper and every `<` in its output has to be one it wrote.
func FuzzMarkdown(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "# h", "- a\n- b", "**b** *i* `c` ~~s~~", "[t](https://e.com)",
		"<script>", "```\nx\n```", "> q", "---", "![i](x)", "[a](javascript:x)",
		"_a_b_", "***", "`` ` ``", "1. a", "\\*", "http://e.com/(a)b", "&amp;",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := string(renderMarkdown(in))
		for _, href := range hrefs(out) {
			if _, _, ok := safeURL(href); !ok {
				t.Fatalf("markdown(%q) wrote href=%q: %s", in, href, out)
			}
		}
		for i := 0; i < len(out); i++ {
			switch out[i] {
			case '>':
				t.Fatalf("markdown(%q) has a bare > at %d: %s", in, i, out)
			case '<':
				end := strings.IndexByte(out[i:], '>')
				if end < 0 {
					t.Fatalf("markdown(%q) has a < that never closes: %s", in, out)
				}
				tag := out[i : i+end+1]
				if !allowedTag.MatchString(tag) {
					t.Fatalf("markdown(%q) wrote %q, which is not in the tag set: %s", in, tag, out)
				}
				i += end
			}
		}
	})
}

// The renderer has its own tests; these are about the wiring. A description
// reaches the reader through three templates, and the one that must not render
// it is the form, where the source is what gets edited.
func TestThePagesRenderTheDescriptionAsMarkdown(t *testing.T) {
	e := seeded(t)
	const src = "Steps:\n\n- pull the **plug**\n- see [the runbook](https://example.com/r)"
	if _, err := e.svc.UpdateCard(t.Context(), e.card.ID, service.CardInput{
		Title: e.card.Title, Description: src,
	}); err != nil {
		t.Fatal(err)
	}

	board := e.do("GET", "/b/demo", nil).Body.String()
	for _, s := range []string{
		`<div class="md`, "<p>Steps:</p>", "<ul><li>pull the <strong>plug</strong></li>",
		`<a href="https://example.com/r" target="_blank"`,
	} {
		if !strings.Contains(board, s) {
			t.Errorf("the board page is missing %q", s)
		}
	}
	if strings.Contains(board, "- pull the **plug**") {
		t.Error("the board page shows the markdown source")
	}

	// The edit form is the exception: it hands back what was typed, so that
	// saving the form again does not rewrite the description as its own markup.
	form := e.do("GET", "/cards/"+string(e.card.ID)+"/edit", nil).Body.String()
	if !strings.Contains(form, "- pull the **plug**") {
		t.Error("the edit form does not show the source")
	}
	if strings.Contains(form, "<strong>plug</strong>") {
		t.Error("the edit form rendered the description into the textarea")
	}

	if err := e.svc.ArchiveCard(t.Context(), e.card.ID); err != nil {
		t.Fatal(err)
	}
	if archive := e.do("GET", "/b/demo/archive", nil).Body.String(); !strings.Contains(archive, "<strong>plug</strong>") {
		t.Errorf("the archive does not render the description:\n%s", archive)
	}
}

// An unclosed bracket used to make the scan read to the end of the description
// and the caller try again at the next character: 20,000 of them is 200 million
// comparisons for one card, on every render.
//
// The budget is ten seconds, which is not a performance target. Linear, this is
// 27ms on a laptop with -race and under a second on the slowest runner this has
// run on; quadratic, it is minutes. Ten seconds separates those two and nothing
// else, which is what the test is for. A tighter budget was a red build on a
// loaded shared runner while the code was correct, and a test that goes red for
// reasons that are not the code is how people learn to ignore red.
func TestUnclosedBracketsAreBounded(t *testing.T) {
	src := strings.Repeat("[", 20000)
	done := make(chan string, 1)
	go func() { done <- string(renderMarkdown(src)) }()
	select {
	case out := <-done:
		if !strings.Contains(out, "[") {
			t.Errorf("the brackets were not printed as themselves: %.80s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rendering 20,000 open brackets took longer than ten seconds, which is the shape of quadratic work")
	}
}

// And a link longer than the bound is not a link, rather than a panic or a
// truncated href.
func TestLinkLongerThanTheBoundIsNotALink(t *testing.T) {
	// A relative destination, so the bare-URL autolinker has nothing to find
	// once the link itself is refused; with an http destination it would link
	// the URL on its own and that is correct, just not what this is asking.
	long := "[" + strings.Repeat("x", maxLinkSpan+10) + "](/somewhere)"
	out := string(renderMarkdown(long))
	if strings.Contains(out, "<a ") {
		t.Errorf("a link that long was rendered: %.120s", out)
	}
	if !strings.Contains(out, "[xxx") {
		t.Errorf("the bracket was not printed as itself: %.80s", out)
	}
	// A real link still is one.
	if !strings.Contains(string(renderMarkdown("[text](https://example.com)")), `href="https://example.com"`) {
		t.Error("an ordinary link stopped working")
	}
}
