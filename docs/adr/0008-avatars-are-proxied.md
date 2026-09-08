# 0008 — Pictures of people are fetched by the server, not by the browser

## Status

Accepted.

## Context

A card shows who it is assigned to as a bubble with their initials, and so does
a comment. The obvious improvement is the person's own picture, and the obvious
way to get one is the URL GitHub publishes:

```
https://github.com/<login>.png?size=128
```

Putting that in an `<img src>` has two costs the rest of the repository already
refuses to pay elsewhere.

The first is privacy. The browser fetches it, so GitHub sees the viewer's IP
address and, from the set of logins requested, which people are on the board
that is open in front of them. That happens on every render, for every viewer,
and it is a request the viewer never asked for. The readme's claim is that
nothing is loaded from a CDN.

The second is the content security policy. `img-src` is `'self' data:`. Loading
a picture from github.com means widening it to a host outside this deployment,
and the redirect target is a different host again
(`avatars.githubusercontent.com`), so it is two.

There is also a plain operational cost: on a network that cannot reach GitHub,
every bubble on the board turns into a broken image.

## Decision

The server fetches the picture and serves it from this origin at
`GET /avatar/{login}`, and the feature is off unless it is configured.

`AVATARS` is a list of `address=github-login` pairs. Only an address in that
list resolves to a picture, and only a login in it is served; anything else is a
404. So the endpoint is not an open proxy for arbitrary GitHub logins, let alone
for arbitrary URLs: the only URL this process ever builds is
`<host>/<configured login>.png`, and a card's assignee cannot change which one.

A fetched picture is cached in memory for a day, a failed fetch for ten minutes.
The failure cache is the load-bearing one. Without it, a login GitHub does not
have is a fresh request for every card that carries it, on every render. The
response is `Cache-Control: private, max-age=86400`, so a board full of cards is
one request per person rather than one per card. The body is capped at 512 KiB,
the content type has to be an image, and the fetch has a five second timeout.

In the page, the picture is laid over the initials bubble instead of replacing
it. An `<img>` with an empty `alt` that fails to load draws nothing, so a
picture that cannot be fetched leaves exactly the bubble that was there before.
The fallback needs no JavaScript and no `onerror` attribute, which the CSP would
have to allow.

## Consequences

Pictures cost one outbound request per person per day, made by the process and
not by the people looking at the board. GitHub learns that this deployment
exists and which logins it was configured with. It learns nothing about who is
looking, or when, or at what.

`img-src` stays `'self' data:`.

The mapping is written by hand. That is tolerable because the set of people on a
self-hosted board is small and stable, and because a GitHub noreply address
already contains the login — `116176330+NicolasHaas@users.noreply.github.com` is
`NicolasHaas` — so the pair is usually a copy of something already on screen.
Reading the login from such an address automatically was considered and dropped:
it would mean serving any login that anybody with board access can type into the
assignee field, which is the open proxy this avoids.

Somebody with no configured picture keeps their initials, which is what every
bubble looked like before. Nothing about the board depends on a picture arriving.
