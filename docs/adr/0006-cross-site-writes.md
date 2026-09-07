# 0006 — Refusing cross-site writes without a token

## Status

Accepted.

## Context

The board has no session of its own. Whatever authenticates a request —
Cloudflare Access, an oauth2-proxy, a reverse proxy on a trusted network — owns
the cookie, and the app never sees a login. That is deliberate and is what ADR
0005 describes.

The consequence is that the app had no CSRF defence and could not easily have
one. A token needs somewhere to keep it, which means a session, which is the
thing the app does not have.

This was not theoretical. Against a running instance, with
`Origin: https://evil.example` and `Sec-Fetch-Site: cross-site`:

| request | result |
|---|---|
| `POST /b/board/cards` | 303, card created |
| `POST /cards/{id}/delete` | 200, card gone |
| `POST /b/board/cards/bulk` `action=delete` | 204, board went from 7 cards to 0 |

The JSON reorder endpoint was not protected by being JSON either. It decodes
the body without checking `Content-Type`, and `text/plain;charset=UTF-8` is a
CORS simple request, so a plain `<form enctype="text/plain">` reaches it with
no preflight.

Whether the cookie rides along is the browser's decision. The Access
application sets `CF_AppSession` with no `SameSite` attribute, so Firefox and
Safari send it on a cross-site top-level form POST, and Chrome sends it inside
its two-minute Lax+POST grace window. The attacker needs no response; a board
wipe is fire-and-forget.

## Decision

Refuse a state-changing request that another origin caused, using
`Sec-Fetch-Site`.

The browser sets that header and a page cannot forge it, so it needs no state
on the server. `same-origin` and `same-site` pass. `none` passes, because it
means a user-initiated navigation — a typed URL, a bookmark — with no other
page involved. Anything else is refused.

When the header is absent, `Origin` is compared against the request's `Host`.
A request with neither passes, which is the case for a command-line client and
for the health probes.

`GET`, `HEAD` and `OPTIONS` are never refused. They change nothing, and
refusing them would break ordinary links into the board.

## Consequences

This is a browser-behaviour defence. It stops a page on another origin; it does
not stop a client that sets its own headers, which is correct, because such a
client is not a confused deputy.

It does not remove the need for `SameSite=Lax` on the Access cookie. That is a
dashboard setting outside this repository, and it is the layer that stops the
request before it ever reaches the app. Both should be in place.

A future session of the app's own would allow a token, at which point this
middleware becomes redundant rather than wrong.
