# 0010 — A card description is markdown, rendered by a subset of our own

## Status

Accepted.

## Context

`docs/adr/0002` wrote the description field down as markdown and left the
rendering to Phase 2. Until now the pages printed the source with
`whitespace-pre-wrap`, so a description that said `**blocked**` showed the
asterisks and a list of three steps was three lines of text with hyphens in
front of them. People write markdown in that box anyway, because the box is a
textarea and that is what everybody types into one.

Rendering it means turning the one long string on the board that a user controls
completely into markup, on a page beside everyone else's cards. So the question
is not only which flavour of markdown, it is who does the escaping.

The library answer is `goldmark`: CommonMark, well tested, widely used. It also
brings roughly 10'000 lines of code, and its own extension and renderer
interfaces, into a repository whose readme sells "one static binary, standard
library" and whose rule is that every new Go dependency gets an ADR. Safe HTML
is not the default there either: `goldmark` passes raw HTML through unless
`html.WithUnsafe` is left off and a sanitiser handles the rest, which in the
usual recipe means a second dependency (`bluemonday`) and a policy to maintain.

The other end is the status quo: no markdown, `whitespace-pre-wrap` forever.
That keeps the escaping in `html/template`, where it is correct by default, and
leaves the asterisks on the card.

## Decision

A subset, rendered by `internal/web/markdown.go`: some 600 lines, comments
included, and no dependency. What it supports:

| Written | Rendered |
| --- | --- |
| a paragraph, blank line between | `<p>` |
| a single newline | `<br>` |
| `#` to `######` | `<h4>` to `<h6>` |
| `-`, `*`, `+` items | `<ul>` |
| `1.`, `1)` items | `<ol>` |
| `> quoted` | `<blockquote>` |
| ` ```fenced``` `, `~~~fenced~~~` | `<pre><code>` |
| `` `code` `` | `<code>` |
| `**bold**`, `__bold__` | `<strong>` |
| `*italic*`, `_italic_` | `<em>` |
| `~~struck~~` | `<del>` |
| `[text](url)`, a bare http(s) URL | `<a>` |
| `---` on its own line | `<hr>` |
| `\*` | a literal `*` |

Four rules decide the rest:

1. **The source is never HTML.** Every byte reaches the output through
   `template.HTMLEscapeString`. There is no branch that copies input through
   unescaped, `<script>` in a description is text, and a fuzz target asserts
   that no tag ever appears in the output which the renderer did not write
   itself.
2. **A destination has to be on the allow list to become an `href`.** `http`,
   `https`, `mailto`, a path starting with a single `/`, or a `#` fragment.
   Everything else — `javascript:`, `data:`, `vbscript:`, `//another.host` — is
   printed as the text that was typed, so a refused link is visible rather than
   silently dropped. An outside link gets `target="_blank"` and
   `rel="noopener noreferrer nofollow"`; every link gets `draggable="false"`,
   because a card face is a drag handle.
3. **A construct that does not close is not a construct.** A lone `*` is an
   asterisk, `**almost` is four characters and an unterminated fence is a code
   block to the end of the description. Nothing is ever swallowed.
4. **A single newline is a line break.** CommonMark folds it into a space and
   asks for two trailing spaces. Nobody means that in a card: the box is where
   an address gets typed over three lines, and it behaved this way before this
   renderer existed. This is the one deliberate departure from the spec.

What is deliberately missing: nested lists (an indented item joins the same
list, one level flat), setext headings, reference links, tables, footnotes, raw
HTML, and images. An image is rendered as a link to it, because fetching a URL
a card names would make the board a proxy for whatever anyone pastes, which is
the question `docs/adr/0008` answered for avatars.

The result is a fragment with no wrapper. The templates put it in an element
with the `md` class and `assets/app.css` styles that class: Tailwind's reset
takes the bullets off a list and the size off a heading, so server-written
markup needs rules of its own.

The JSON API keeps returning the source, and so does the edit form. What was
typed is what gets edited and what a script reads.

## Consequences

- The card face and the archive show formatted descriptions. The board is
  denser: a three-step description is a list, not three lines.
- Search still matches the source, so `label:` in a description is found by
  searching for the characters that are in the box.
- The subset is ours to extend, and a description written elsewhere in full
  CommonMark renders approximately. A table pasted into a card shows its pipes.
- No new dependency, and the escaping stays in one file with a fuzz target on
  it. If markdown grows past what a card needs, this ADR is the thing to
  supersede, and `goldmark` plus a sanitiser is the shape that replaces it.
- `MaxDescription` bounds the work: rendering is a single pass over a string the
  service already caps.
