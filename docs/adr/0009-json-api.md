# 0009 — A JSON API beside the pages

## Status

Accepted.

## Context

Everything the board can do, it does through `internal/service`. The pages in
`internal/web` parse a form, call one service method and render a template.

Scripting a board today means using those page routes: posting
`application/x-www-form-urlencoded` to a URL that was designed for a form,
guessing from a `303` whether the write landed, and scraping an HTML fragment
to read back the id of the card just created. Three things I actually wanted
were a card from a cron job, a weekly count over a shell pipeline, and a fresh
install seeded with its columns. All three are a page away from working and a
parser away from reliable.

The page routes cannot become that interface. They answer what htmx needs,
which is a fragment or a `204` with a header, and their URLs are the page's
vocabulary: `/b/{board}` is short because a person types it. A program needs a
body it can decode and a URL that will not be renamed for the sake of a nicer
bookmark.

## Decision

A second surface, `internal/api`, under `/api/v1/`, over the same
`service.Kanban`.

It is a package and not a set of handlers in `internal/web` because the two
translate in opposite directions: one renders HTML for a browser, the other
encodes JSON for a program. They share the layer underneath, and that is the
only place a rule about a WIP limit or a comment's author is written down.

The path is versioned and the page routes are not. A page URL breaks a
bookmark; `/api/v1/boards` breaks a cron job nobody is going to notice for a
week. Within `v1` a field keeps its name, a status code keeps its meaning, and
anything that cannot be done under those terms waits for `v2`.

There are no tokens. The API grants what the board grants, which is everything
to whoever can reach the port, and a deployment protects it the way it protects
the pages: with the proxy from ADR 0005 or Cloudflare Access in front. Identity
comes from the same middleware and is used for the same two things, an
assignment and a comment's author. A token scheme means a token table, rotation
and a screen to manage it, and a board with one user table's worth of people
does not need any of it.

It is mounted on the page mux by `web.WithAPI`, inside the middleware that was
already there, so both kinds of caller get one body cap, one cross-site check
and one log line. `web` takes it as an `http.Handler` and never imports the
package.

### The document is written by hand and checked by tests

`internal/api/openapi.json` is an OpenAPI 3.1 document, in the repository, and
served at `GET /api/v1/openapi.json`. It is not generated, and there is no
swagger-ui: that is about a megabyte of vendored JavaScript, and the reason
this project has an asset tree it can read is that it has none.

It is JSON and not YAML so that a test can read it with `encoding/json`. A
generator and a YAML parser are both dependencies, and every dependency here
costs an ADR of its own (0001).

A hand-written contract that nothing checks describes last month's API, so four
tests hold it to the code:

- every route registered in `api.go` is a path in the document, and every path
  in the document is a registered route, with the registrations counted so a
  call the regexp cannot read fails loudly instead of being skipped. This is
  the rule `internal/web/api_doc_test.go` already applies to `docs/api.md`.
- every property of every schema is a field of the Go type that fills it, in
  both directions, and for a response the required list is the fields without
  `omitempty`.
- the lengths and counts in the document are the `service.Max…` constants.
- every response the suite provokes is validated against the schema declared
  for it, by a JSON Schema subset validator in the test package that reports a
  keyword it does not understand rather than passing it.

## Consequences

Two surfaces answer for the same board, and a new capability has two places to
appear rather than one. A field added to a card is a template change and a
schema change, and the second test above is what turns forgetting into a
failure.

The document is as trustworthy as the tests are thorough. They check names,
shapes and limits; they do not check that a description is still true. A
sentence about what a `400` means can rot, and only a reader will catch it.

Anything reachable at `/api/v1/` is reachable by anything that can reach the
port, so putting this board on the open internet without something in front of
it is now worse than it was.

`v1` is a promise, and the way to keep it cheap is to keep the surface small.
Thirty routes covering what the pages already do is the whole of it; a request
for a filter or a report is a query parameter on one of them before it is a
route of its own.
