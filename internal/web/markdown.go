package web

import (
	"html/template"
	"strings"
)

// A card description is markdown, and this renders it.
//
// It is a subset, written by hand, and that is the decision rather than an
// apology for one: a description is a paragraph, a list of steps and a link,
// and a full CommonMark implementation is a dependency of some 10'000 lines
// that has to be trusted with the one string on the board a user controls
// completely. What is supported is in docs/adr/0010; the short version is
// paragraphs, `#` to `###`, `-`/`*`/`+` and `1.` lists, `>` quotes, fenced and
// inline code, `**bold**`, `*italic*`, `~~struck~~`, `[text](url)` and a bare
// http(s) URL. What is not supported is rendered as the characters that were
// typed, so nothing is ever swallowed.
//
// Two rules hold the whole file up:
//
//   - The source is never HTML. Every byte of it reaches the output through
//     template.HTMLEscapeString, so `<script>` in a description is text. There
//     is no path that copies input into the output unescaped, and a fuzz test
//     asserts no tag ever appears that this file did not write.
//   - A construct that does not close is not a construct. `*` with no partner
//     is an asterisk, and an unterminated fence is a code block to the end of
//     the description rather than a swallowed rest.
//
// The output is a fragment: block elements, no wrapper. The caller puts it in
// an element with the `md` class, which app.css styles.
func renderMarkdown(src string) template.HTML {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			i++
		case fenceAt(line) != "":
			i = writeCodeBlock(&b, lines, i)
		case isThematicBreak(line):
			b.WriteString("<hr>")
			i++
		default:
			if level, text, ok := headingAt(line); ok {
				// h4 and below, because a description sits inside a page that
				// already has its own heading and a card is not a document.
				tag := [...]string{"h4", "h5", "h6"}[min(level, 3)-1]
				b.WriteString("<" + tag + ">")
				writeInline(&b, text)
				b.WriteString("</" + tag + ">")
				i++
				continue
			}
			if _, _, ok := listItemAt(line); ok {
				i = writeList(&b, lines, i)
				continue
			}
			if _, ok := quoteAt(line); ok {
				i = writeQuote(&b, lines, i)
				continue
			}
			i = writeParagraph(&b, lines, i)
		}
	}
	return template.HTML(b.String())
}

// --- blocks -------------------------------------------------------------------

// startsBlock reports whether a line inside a paragraph ends it by being
// something else. A list or a heading directly under a line of prose is how
// people write, so it does not need the blank line CommonMark asks for.
func startsBlock(line string) bool {
	if strings.TrimSpace(line) == "" || fenceAt(line) != "" || isThematicBreak(line) {
		return true
	}
	if _, _, ok := headingAt(line); ok {
		return true
	}
	if _, _, ok := listItemAt(line); ok {
		return true
	}
	_, ok := quoteAt(line)
	return ok
}

// indent is how much leading space a line may have and still be the thing it
// looks like. Four spaces mean an indented code block in CommonMark, which this
// subset does not have; three is the limit everywhere else, and keeping it here
// means a list nested one level does not silently become a paragraph.
const indent = 3

// trimIndent removes up to indent spaces, and reports whether the line was
// indented further than that.
func trimIndent(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	return trimmed, len(line)-len(trimmed) > indent
}

// fenceAt returns the fence a line opens, "" if it opens none. The fence is
// returned rather than a bool because the block ends on the same character: a
// ``` block holds ~~~ as text and the other way round.
func fenceAt(line string) string {
	trimmed, deep := trimIndent(line)
	if deep {
		return ""
	}
	for _, fence := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, fence) {
			return fence
		}
	}
	return ""
}

// writeCodeBlock renders the fenced block that starts at lines[i] and returns
// the line after it. The info string after the fence is read and dropped: no
// highlighter runs here, so the language would only be an attribute nothing
// looks at.
func writeCodeBlock(b *strings.Builder, lines []string, i int) int {
	fence := fenceAt(lines[i])
	i++
	b.WriteString("<pre><code>")
	for ; i < len(lines); i++ {
		if trimmed, deep := trimIndent(lines[i]); !deep && strings.HasPrefix(trimmed, fence) {
			i++
			break
		}
		b.WriteString(template.HTMLEscapeString(lines[i]))
		b.WriteString("\n")
	}
	b.WriteString("</code></pre>")
	return i
}

// isThematicBreak is three or more of -, * or _ on a line of their own.
func isThematicBreak(line string) bool {
	trimmed, deep := trimIndent(line)
	if deep {
		return false
	}
	trimmed = strings.TrimRight(trimmed, " \t")
	for _, c := range []string{"-", "*", "_"} {
		if len(trimmed) >= 3 && strings.Trim(trimmed, c) == "" {
			return true
		}
	}
	return false
}

// headingAt reads an ATX heading. The hashes have to be followed by a space,
// so #1 in a description is the number and not a heading, and the closing
// hashes CommonMark allows are trimmed.
func headingAt(line string) (level int, text string, ok bool) {
	trimmed, deep := trimIndent(line)
	if deep {
		return 0, "", false
	}
	hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	if hashes == 0 || hashes > 6 {
		return 0, "", false
	}
	rest := trimmed[hashes:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	if closing := strings.TrimRight(rest, "#"); closing != rest &&
		(closing == "" || strings.HasSuffix(closing, " ")) {
		rest = strings.TrimSpace(closing)
	}
	return hashes, rest, true
}

// listItemAt reads one list item. ordered is a 1. or 1) item, which is a
// different element, and the text is what follows the marker.
func listItemAt(line string) (text string, ordered, ok bool) {
	trimmed, deep := trimIndent(line)
	if deep {
		return "", false, false
	}
	// A bullet needs the space: *bold on a line of its own is emphasis, and
	// -1 is a number. A marker with nothing after it is an empty item.
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			return strings.TrimSpace(trimmed[2:]), false, true
		}
	}
	if trimmed == "-" || trimmed == "*" || trimmed == "+" {
		return "", false, true
	}
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	// Nine digits is CommonMark's limit for a marker, and past it a year with
	// a full stop after it stops looking like a list.
	if digits == 0 || digits > 9 || digits+1 > len(trimmed) {
		return "", false, false
	}
	if c := trimmed[digits]; c != '.' && c != ')' {
		return "", false, false
	}
	rest := trimmed[digits+1:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return "", false, false
	}
	return strings.TrimSpace(rest), true, true
}

// writeList renders the run of items that starts at lines[i]. A blank line ends
// the list rather than making it loose: on a card the difference between a tight
// and a loose list is a paragraph of air nobody asked for.
//
// Items do not nest. An indented item is an item of the same list, which is the
// failure mode that loses the least: the text is all there, one level flat.
func writeList(b *strings.Builder, lines []string, i int) int {
	_, ordered, _ := listItemAt(lines[i])
	tag := "ul"
	if ordered {
		tag = "ol"
	}
	b.WriteString("<" + tag + ">")
	for ; i < len(lines); i++ {
		text, isOrdered, ok := listItemAt(lines[i])
		if !ok || isOrdered != ordered {
			break
		}
		b.WriteString("<li>")
		writeInline(b, text)
		b.WriteString("</li>")
	}
	b.WriteString("</" + tag + ">")
	return i
}

// quoteAt reads a > line. The marker may be followed by a space, and a bare >
// is an empty line inside the quote.
func quoteAt(line string) (text string, ok bool) {
	trimmed, deep := trimIndent(line)
	if deep || !strings.HasPrefix(trimmed, ">") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(trimmed, ">"), " "), true
}

// writeQuote renders consecutive > lines as one quote. What is inside is
// paragraph text, so the lines are joined the way a paragraph joins them.
func writeQuote(b *strings.Builder, lines []string, i int) int {
	var text []string
	for ; i < len(lines); i++ {
		line, ok := quoteAt(lines[i])
		if !ok {
			break
		}
		text = append(text, line)
	}
	b.WriteString("<blockquote><p>")
	writeSoftLines(b, text)
	b.WriteString("</p></blockquote>")
	return i
}

// writeParagraph renders lines up to the next blank line or block, and returns
// the line after it.
func writeParagraph(b *strings.Builder, lines []string, i int) int {
	var text []string
	for ; i < len(lines); i++ {
		if len(text) > 0 && startsBlock(lines[i]) {
			break
		}
		text = append(text, lines[i])
	}
	b.WriteString("<p>")
	writeSoftLines(b, text)
	b.WriteString("</p>")
	return i
}

// writeSoftLines joins lines with a <br>.
//
// CommonMark folds a single newline into a space and wants two trailing spaces
// for a break. Nobody writing a card description means that: the box is where
// they type an address over three lines, and before this renderer existed the
// description was displayed with whitespace-pre-wrap, so every newline was a
// break. Keeping that is the one deliberate departure from the spec.
func writeSoftLines(b *strings.Builder, lines []string) {
	for n, line := range lines {
		if n > 0 {
			b.WriteString("<br>")
		}
		writeInline(b, strings.TrimRight(line, " \t"))
	}
}

// --- inline -------------------------------------------------------------------

// writeInline renders one line's worth of markdown. Everything that is not a
// construct is escaped text, so the default branch of the switch is the one
// that carries the safety: it writes one escaped byte and moves on.
func writeInline(b *strings.Builder, s string) {
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]):
			// A backslash escape is how a description says a literal asterisk.
			b.WriteString(template.HTMLEscapeString(s[i+1 : i+2]))
			i += 2
		case s[i] == '`':
			if n := writeCodeSpan(b, s[i:]); n > 0 {
				i += n
				continue
			}
			b.WriteString("`")
			i++
		case s[i] == '[':
			if n := writeLink(b, s[i:]); n > 0 {
				i += n
				continue
			}
			b.WriteString("[")
			i++
		case s[i] == '!' && i+1 < len(s) && s[i+1] == '[':
			// An image is a link to it. Fetching a URL a card names would make
			// the board a proxy for whatever anyone pastes, which is the
			// question docs/adr/0008 answered for avatars with "only what the
			// operator configured".
			if n := writeLink(b, s[i+1:]); n > 0 {
				i += n + 1
				continue
			}
			b.WriteString("!")
			i++
		case hasDelimiter(s, i, "**"), hasDelimiter(s, i, "__"):
			i += writeEmphasis(b, s, i, s[i:i+2], "strong")
		case hasDelimiter(s, i, "~~"):
			i += writeEmphasis(b, s, i, "~~", "del")
		case hasDelimiter(s, i, "*"), hasDelimiter(s, i, "_"):
			i += writeEmphasis(b, s, i, s[i:i+1], "em")
		default:
			// Only where a scheme could start, so the scan below is not run
			// once per byte of every description.
			if s[i] == 'h' || s[i] == 'H' {
				if n := writeAutolink(b, s, i); n > 0 {
					i += n
					continue
				}
			}
			b.WriteString(template.HTMLEscapeString(s[i : i+1]))
			i++
		}
	}
}

// hasDelimiter reports whether an emphasis run starts at i.
//
// The underscore is held to a word boundary and the asterisk is not, which is
// CommonMark's rule and the reason snake_case_names survive being written in a
// description.
func hasDelimiter(s string, i int, delim string) bool {
	if !strings.HasPrefix(s[i:], delim) {
		return false
	}
	if delim != "_" && delim != "__" {
		return true
	}
	return i == 0 || !isWordByte(s[i-1])
}

// writeEmphasis renders a run closed by the same delimiter and returns how many
// bytes it consumed, or the delimiter as text when nothing closes it.
func writeEmphasis(b *strings.Builder, s string, i int, delim, tag string) int {
	rest := s[i+len(delim):]
	for at := 0; at < len(rest); {
		end := strings.Index(rest[at:], delim)
		if end < 0 {
			break
		}
		end += at
		// A closing underscore has the same word-boundary rule as an opening
		// one, so a_b_c is not emphasis and _a_b is not either.
		if (delim == "_" || delim == "__") && end+len(delim) < len(rest) && isWordByte(rest[end+len(delim)]) {
			at = end + len(delim)
			continue
		}
		if end == 0 {
			break // ** with nothing inside it
		}
		// The closer is taken from the end of its run, so that the inner run of
		// ***late*** closes inside the outer one and both tags come out. Without
		// this the leftmost ** wins and the last asterisk is left over.
		run := len(rest[end:]) - len(strings.TrimLeft(rest[end:], delim[:1]))
		end += run - len(delim)
		b.WriteString("<" + tag + ">")
		writeInline(b, rest[:end])
		b.WriteString("</" + tag + ">")
		return len(delim) + end + len(delim)
	}
	b.WriteString(template.HTMLEscapeString(delim))
	return len(delim)
}

// writeCodeSpan renders `code` and returns how many bytes it consumed, 0 when
// the span does not close. Nothing inside is markdown, which is the point of
// it: a description explaining the syntax has to be able to show it.
func writeCodeSpan(b *strings.Builder, s string) int {
	// A run of backticks opens a span that only a run of the same length
	// closes, so `` a ` b `` holds a backtick.
	ticks := len(s) - len(strings.TrimLeft(s, "`"))
	fence := s[:ticks]
	end := strings.Index(s[ticks:], fence)
	if end < 0 {
		return 0
	}
	code := s[ticks : ticks+end]
	// One space either side is padding for a span that starts or ends with a
	// backtick, and is not part of the code.
	if len(code) > 1 && strings.HasPrefix(code, " ") && strings.HasSuffix(code, " ") {
		code = code[1 : len(code)-1]
	}
	b.WriteString("<code>")
	b.WriteString(template.HTMLEscapeString(code))
	b.WriteString("</code>")
	return ticks + end + ticks
}

// writeLink renders [text](url) and returns how many bytes it consumed, 0 when
// what follows the bracket is not a link after all.
func writeLink(b *strings.Builder, s string) int {
	text, rest, ok := balanced(s, '[', ']')
	if !ok || !strings.HasPrefix(rest, "(") {
		return 0
	}
	dest, after, ok := balanced(rest, '(', ')')
	if !ok {
		return 0
	}
	url, title := splitDest(dest)
	href, external, ok := safeURL(url)
	if !ok {
		// A refused destination is not a silent nothing: the text and the URL
		// are both printed, so a description with a javascript: link still
		// says what it said, and whoever wrote it can see why it is not one.
		b.WriteString("[")
		writeInline(b, text)
		b.WriteString("](")
		// The destination as typed, not parsed further: a refused URL that has
		// an acceptable one inside it would otherwise become a link to that,
		// which reads as if the refusal had not happened.
		b.WriteString(template.HTMLEscapeString(dest))
		b.WriteString(")")
		return len(s) - len(after)
	}
	writeAnchor(b, href, title, external, func() { writeInline(b, text) })
	return len(s) - len(after)
}

// writeAutolink renders a bare http(s) URL and returns how many bytes it
// consumed. People paste URLs into descriptions and expect them to be links;
// this is the whole of that expectation, and no other scheme is guessed at.
func writeAutolink(b *strings.Builder, s string, i int) int {
	if i > 0 && isWordByte(s[i-1]) {
		return 0
	}
	rest := s[i:]
	if !hasPrefixFold(rest, "http://") && !hasPrefixFold(rest, "https://") {
		return 0
	}
	end := len(rest)
	for at := 0; at < len(rest); at++ {
		if isURLByte(rest[at]) {
			continue
		}
		end = at
		break
	}
	// Trailing punctuation belongs to the sentence, not to the URL, and a
	// closing bracket only belongs to it if it was opened inside it.
	for end > 0 {
		last := rest[end-1]
		if strings.IndexByte(".,;:!?'\"", last) >= 0 ||
			(last == ')' && strings.Count(rest[:end], "(") < strings.Count(rest[:end], ")")) {
			end--
			continue
		}
		break
	}
	url := rest[:end]
	if strings.HasSuffix(url, "://") {
		return 0 // a scheme and no host
	}
	href, external, ok := safeURL(url)
	if !ok {
		return 0
	}
	writeAnchor(b, href, "", external, func() { b.WriteString(template.HTMLEscapeString(url)) })
	return end
}

// writeAnchor is the one place a link is written, so the attributes on it are
// decided once.
//
// draggable="false" for the same reason the label chip on a card face carries
// it: the card is a drag handle, and a link inside one is something the browser
// would rather drag than let SortableJS move the card. target for an outside
// URL because losing the board to a click on a link in a description is not
// what anybody meant by it, and rel because a card is user content: the page
// it opens has no business reaching back into the board.
func writeAnchor(b *strings.Builder, href, title string, external bool, text func()) {
	b.WriteString(`<a href="` + href + `"`)
	if title != "" {
		b.WriteString(` title="` + template.HTMLEscapeString(title) + `"`)
	}
	if external {
		b.WriteString(` target="_blank" rel="noopener noreferrer nofollow"`)
	}
	b.WriteString(` draggable="false">`)
	text()
	b.WriteString("</a>")
}

// safeURL decides whether a destination may become an href, and returns it
// escaped for the attribute.
//
// An allow list, because the interesting schemes are the ones nobody thinks of:
// javascript: is the one everybody remembers, data: carries a whole document,
// and vbscript: is still there. http, https and mailto are what a card
// description wants, and a path is how it links to another board.
func safeURL(dest string) (href string, external, ok bool) {
	url := strings.TrimSpace(dest)
	if url == "" {
		return "", false, false
	}
	if strings.ContainsAny(url, " \t\"'<>`") {
		// A destination with a space in it is either not a URL or is trying to
		// be more than one attribute. Angle-bracketed destinations, which is
		// how CommonMark writes those, are not in this subset.
		return "", false, false
	}
	switch {
	case hasPrefixFold(url, "http://"), hasPrefixFold(url, "https://"),
		hasPrefixFold(url, "mailto:"):
		return template.HTMLEscapeString(url), true, true
	case strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//"):
		// A board of its own, or a card. Not // , which is a URL on another
		// host with the scheme left to the browser.
		return template.HTMLEscapeString(url), false, true
	case strings.HasPrefix(url, "#"):
		return template.HTMLEscapeString(url), false, true
	}
	return "", false, false
}

// splitDest separates a destination from the "title" some links carry.
func splitDest(dest string) (url, title string) {
	dest = strings.TrimSpace(dest)
	for _, quote := range []string{`"`, `'`} {
		if at := strings.Index(dest, " "+quote); at >= 0 && strings.HasSuffix(dest, quote) {
			return dest[:at], strings.Trim(dest[at+1:], quote)
		}
	}
	return dest, ""
}

// balanced reads the run that starts at s[0] == open up to the matching close,
// and returns what was inside it and what follows.
//
// Counting rather than searching, because a URL holds brackets: the parentheses
// in a Wikipedia link close each other, and the link's own closing one is the
// last.
func balanced(s string, open, close byte) (inside, after string, ok bool) {
	if len(s) == 0 || s[0] != open {
		return "", s, false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
		case s[i] == open:
			depth++
		case s[i] == close:
			if depth--; depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", s, false
}

// hasPrefixFold is strings.HasPrefix for a scheme, which is case-insensitive.
// Written out rather than lowercasing the rest of the line, because that
// allocated a copy of the description for every character of it.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte(`!"#$%&'()*+,-./:;<=>?@[\]^_`+"`"+`{|}~`, c) >= 0
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// isURLByte is the set a bare URL may be made of. It is deliberately wider than
// the characters a URL is allowed to contain unencoded, because what is being
// found here is where the URL stops, and safeURL is what decides whether it may
// be a link.
func isURLByte(c byte) bool {
	if isWordByte(c) {
		return true
	}
	return strings.IndexByte("-.:/?#[]@!$&'()*+,;=%~", c) >= 0
}
