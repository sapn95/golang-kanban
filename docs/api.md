# Endpoints

Every route the server answers, all 36 of them, registered in one block in
`internal/web/server.go`. A test compares this file against that block and
fails when either side has something the other does not, so a route cannot be
added or removed without the table changing with it.

The last of those 36 is the JSON API, which brings its own thirty-one routes under
`/api/v1/` and its own document; the [table below](#the-json-api) gives it one
row. This file is about the page routes.

Those have no OpenAPI document, and neither half has swagger-ui. Almost every
response here is an HTML fragment, a redirect or a `204` with an htmx header,
so a spec would say `type: string` for nearly all of them, and swagger-ui is
about a megabyte of vendored JavaScript in a project whose selling point is
that it has none. `/api/v1/openapi.json` is a file in the repository,
[readable as it is](../internal/api/openapi.json).

## What is true of every request

Reads are `GET`, everything else is `POST`. There is no `PUT` or `DELETE`,
because a browser form cannot send one and this board works without
JavaScript for anything but drag-and-drop.

Writes send `application/x-www-form-urlencoded`, except the card order, which
sends JSON. Bodies are capped at 1 MiB before a handler reads them.

A write from another origin is refused with `403`. The guard reads
`Sec-Fetch-Site`, falls back to `Origin`, and lets a request through that
carries neither, which is how `curl` reaches it; see
[0006](adr/0006-cross-site-writes.md).

htmx changes what a write answers, not what it does. With `HX-Request: true`
the response is the fragment to swap in; without it the same write ends in a
`303` to a page, so a form still works with JavaScript off. Where no fragment
can express the change, the answer is `204` with `HX-Refresh: true` and the
browser reloads the page.

A response is `Cache-Control: no-store` unless its row below names another
policy, which the two routes that serve bytes rather than a page do.

## Boards

| Route | Sends | Answers |
|-------|-------|---------|
| `GET /` | | The list of boards, or `303` to `/b/{slug}` when there is only one |
| `POST /boards` | `name` | `303` to the new board. `400` and the list again, with the reason, when the name is empty or already taken |
| `GET /b/{board}` | `?q=` to search, optional | The board. `404` for a slug that does not exist |
| `POST /b/{board}/layout` | `layout=columns` or `rows` | `303` back to the board |
| `POST /b/{board}/sla` | `response_hours`, `days` (once per day), `start`, `end`, `zone` | `303` back to settings. `400` re-renders the page with the reason |

`q` matches a bare word against the title, the description and the label
names, and understands `label:`, `assignee:`, `due:` and `is:archived`, with
`"quoted phrases"`. It lives in the URL so a result can be linked to.

A word that matches none of the three literally is tried once more as a typo,
against the words of those same fields: one edit for a word of four letters or
more, two from eight, and a swapped pair of letters counts as one. Under four
letters there is no tolerance, and a `"quoted phrase"` is always literal. The
same `q` works on `GET /b/{board}/archive`, where it is scoped to the archive
whether or not it says `is:archived`.

The response-time promise is one per board: how many office hours a card may
sit untouched before the badge on it turns. `days` is sent once per office day,
`start` and `end` are `HH:MM` read in `zone`, and `response_hours=0` switches
the whole thing off. An `end` of `00:00` is the midnight that ends the day, for
a desk that never closes. The clock does not run on an archived card, or in a
column marked `stops_clock` on the settings page.

## Cards

| Route | Sends | Answers |
|-------|-------|---------|
| `POST /b/{board}/cards` | `title`, `column`, and optionally `description`, `due_date`, `assignee`, `subtasks`, `labels` (once per label) | The new card, with `HX-Retarget: #cards-{column}` and `HX-Reswap: beforeend` so it lands in the right column. `303` to the board without htmx |
| `GET /cards/{id}` | | The card face |
| `GET /cards/{id}/edit` | | The edit form with the card's comments |
| `POST /cards/{id}` | the fields above, plus `column` to move it | The card face. `204` with `HX-Refresh: true` when the column changed, because the card now belongs to a list this response cannot reach. `303` to the board without htmx |
| `POST /cards/{id}/assignee` | `assignee`: an address, `@me`, or empty to unassign | The card face, or `303` without htmx. `403` on `@me` when nobody is signed in |
| `POST /cards/{id}/due` | `due_date`: `YYYY-MM-DD`, or empty to take the date off | The card face, or `303` without htmx. `400` on anything else |
| `POST /cards/{id}/labels/{label}/toggle` | | The card face with that label put on or taken off. The label stays on the board either way |
| `POST /cards/{id}/archive` | | `200` and an empty body. The caller removes the row |
| `POST /cards/{id}/delete` | | `200` and an empty body |
| `POST /cards/{id}/restore` | `?board=` the slug to return to | `204` with `HX-Redirect: /b/{slug}` |
| `GET /b/{board}/archive` | `?q=` to search, optional | The archived cards, each with a restore button |
| `POST /b/{board}/cards/bulk` | `action=move\|assign\|archive\|delete`, `ids` (once per card), `target` (a column id for `move`, an address or `@me` for `assign`) | `204` with `HX-Refresh: true`. `403` on `target=@me` when nobody is signed in |
| `POST /b/{board}/columns/{column}/order` | JSON `{"order":["card-id", …]}` | `OK` as text. `409` when a WIP limit refuses the move |

The order request is the whole order of one column, and any card named in it
moves into that column from wherever it was. A drag between two columns
therefore sends one request, for the destination.

## Comments

| Route | Sends | Answers |
|-------|-------|---------|
| `POST /cards/{id}/comments` | `body` | The new comment, and the card face as an out-of-band swap so the count on the board behind the modal keeps up |
| `POST /comments/{id}/delete` | | The card face out of band and nothing else, which leaves the empty string to replace the comment's own row. `403` for anybody but the author |

Comments cannot be edited; [0007](adr/0007-comments-are-append-only.md) says
why.

## Board settings

Every write here ends on the settings page, so a refused one can show the
reason where the form is. A validation error re-renders the page with the
message and the appropriate status rather than redirecting.

| Route | Sends | Answers |
|-------|-------|---------|
| `GET /b/{board}/settings` | | The board's columns and labels, with how many cards use each |
| `GET /b/{board}/labels` | | `301` to the settings page. This URL shipped before that page existed |
| `POST /b/{board}/labels` | `name`, `color` | `303` to settings. `400` or `409` re-renders it with the reason |
| `POST /b/{board}/labels/{id}` | `name`, `color` | as above |
| `POST /b/{board}/labels/{id}/delete` | | `204` with `HX-Refresh: true` over htmx, `303` to settings otherwise. The label comes off every card that carries it |
| `POST /b/{board}/columns` | `name`, `wip_limit` (empty or `0` for none), `stops_clock` | `303` to settings |
| `POST /b/{board}/columns/{id}` | `name`, `wip_limit`, `stops_clock` | `303` to settings |
| `POST /b/{board}/columns/{id}/delete` | `move_to`: the column the cards go to, empty to delete them with it | `303` to settings |
| `POST /b/{board}/columns/{id}/move` | `direction=up\|down` | `303` to settings. A move off either end changes nothing |

## The JSON API

| Route | Sends | Answers |
|-------|-------|---------|
| `ANY /api/v1/{path}` | JSON | JSON. `GET /api/v1/` lists the entry points and `GET /api/v1/openapi.json` is the whole contract; see [0009](adr/0009-json-api.md) |

It calls the same service methods these pages call, so the two cannot disagree
about what a WIP limit means or who may delete a comment. What it adds is a
caller that is a program: `PUT` and `DELETE` are real methods there, a response
is an object rather than a fragment, and an unknown field in a body is a `400`
that names it instead of a card with no title.

It grants what the board grants, which is everything to whoever can reach the
port. There are no tokens, so whatever protects the pages, a proxy or
Cloudflare Access, protects this too.

## Files, pictures and operations

| Route | Sends | Answers |
|-------|-------|---------|
| `GET /assets/{path}` | | The embedded file, `public, max-age=31536000, immutable`. The URL carries the digest of the tree (`app.js?v=…`), so that is honest. No directory listing |
| `GET /avatar/{login}` | | The picture, `private, max-age=86400`. Registered only when `AVATARS` names somebody, and `404` for a login it does not name; see [0008](adr/0008-avatars-are-proxied.md) |
| `GET /healthz` | | `ok`. The process is up |
| `GET /readyz` | | `ready`, or `503 not ready` when the database does not answer |
| `GET /version` | | `kanban {version} (commit {sha})`, which is how you find out whether a deploy landed |
| `GET /favicon.ico` | | `204` |

## What a failure looks like

An error is `text/plain` and one line, because every write is either swapped
into the page by htmx or followed by a redirect, and a stack of HTML in place
of a card is worse than a sentence. Under `/api/v1/` the same failure is
`{"error": "…"}`, including the two the middleware answers before the API
handler sees the request: a cross-site write and a panic.

| Status | When |
|--------|------|
| `400` | A field the service refuses, a body that is not a form, JSON that does not parse, a body over 1 MiB |
| `403` | A cross-site write, `@me` with nobody signed in, deleting somebody else's comment |
| `404` | No such board, card, comment, label or column |
| `409` | A name that is taken, or a move a WIP limit refuses |
| `500` | Anything else. The detail is logged, not returned |
| `503` | `/readyz` only, when the database does not answer |

Who is making the request comes from `AUTH_MODE`:
[0005](adr/0005-request-identity.md) covers the three modes. With `none` there
is no signed-in person, which is why the endpoints that resolve `@me` answer
`403`.
