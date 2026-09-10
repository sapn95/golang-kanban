# 0005 — Request identity: none, proxy, or a verified assertion

## Status

Accepted.

## Context

The board had no users. Every request was anonymous, which was fine while it
served one person on a LAN, and stops being fine the moment anything has to
answer "by whom": an assignee, a comment, an audit line.

Deployments differ in what they can be told about the caller.

- Run locally or on a trusted network, there is no identity at all and asking
  for one would be friction with nothing behind it.
- Behind a reverse proxy that has already authenticated the caller
  (oauth2-proxy, an ingress with an auth annotation), the answer arrives in a
  header.
- Behind Cloudflare Access, the answer arrives as a JWT signed by Cloudflare.

The last two look the same from a handler's point of view and are not the
same thing. A header is a claim by whatever wrote it. Anything that can open a
socket to the port can set `X-Forwarded-Email` to any value, so trusting one
is only sound when the port is genuinely unreachable except through the proxy
— a property of the deployment, not of the app, and one that silently stops
holding the day someone adds a NodePort.

## Decision

`AUTH_MODE` selects one of three modes, defaulting to `none`.

`proxy` reads a configured header. It is documented as trusting the network.

`access` verifies `Cf-Access-Jwt-Assertion` itself: RS256 signature against
the team's published keys, plus `aud`, `iss`, `exp` and `nbf`. It is sound
even if the port is exposed, because a forged assertion does not verify.

Handlers do not branch on the mode. They call `identity.FromContext`, which
returns the zero `User` when nobody is identified, so the anonymous case needs
no special path.

### Identity is not authorization

None of the three modes refuses anybody. They say who a caller is; a caller who
is nobody is served anyway, with every read and write the board has. That is the
right default for a board on a LAN, and it is a hole wherever the port is
reachable by a second route — a NodePort beside a tunnel, a published port
beside a proxy — because the assertion only arrives on the route that mints it.
A deployment reading "we run access mode" as "the board is behind a gate" is
reading something the app never claimed.

`AUTH_REQUIRED=true` makes it claim it: a request with no identity gets a `403`
instead of the page. It needs `proxy` or `access`, since nobody is ever
identified in `none`, and `FromEnv` refuses that combination on start rather
than at the first request, where it would look like an outage.

`/healthz` and `/readyz` stay open whatever it says. A kubelet has no assertion
to present, and a liveness probe that gets a `403` restarts a healthy container
in a loop. They answer nothing about a board, which is what makes them safe to
leave out, and the list is a function rather than an exported set so that what
goes unauthenticated cannot be widened from somewhere else. `/version` is not on
it: it names the build, and the footer already says that to whoever is signed in.

The verifier is written against the standard library. The token shape is fixed
and narrow, and a dependency that parses attacker-controlled input is one that
has to be watched for as long as the project lives.

### Checks that are load-bearing

The algorithm is pinned to RS256. Accepting the header's own `alg` is how
`none` tokens and HMAC-keyed-on-the-public-key forgeries get in.

`aud` is required in configuration, not optional. A Cloudflare team signs
assertions for every application under it with the same keys, so without an
audience check a token for any other application on the team opens this one.

A `kid` that is not in the cached key set forces a refetch rather than a
rejection, because that is exactly what a key rotation looks like and waiting
out the cache TTL would be an outage. Conversely, if the certs endpoint cannot
be reached, a key that verified a moment ago keeps working.

## Consequences

A failed verification logs and continues anonymously rather than returning
403. The alternative turns a Cloudflare-side key rotation, or an expired tab
left open overnight, into a hard failure on a board that is already behind
Access. The request still reaches an app that knows nobody is signed in, which
is the same state as `none`.

With `AUTH_REQUIRED=true` that is the trade the deployment has chosen: the same
rotation now refuses the page rather than drawing it signed out. The failure is
loud instead of quiet, which is the point of turning it on, and it is why the
flag is off by default rather than on.

`access` mode makes a network call for the key set. It is cached for an hour,
so it is one request per hour per process, and a failure to refresh degrades
to the previous keys rather than to an outage.

Nothing yet writes the identity down. Assignee and comments are the reason
this exists, and they are separate changes on top.
