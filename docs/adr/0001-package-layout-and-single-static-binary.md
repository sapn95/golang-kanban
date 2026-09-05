# 0001 — Package layout and a single static binary

Status: proposed
Date: 2026-09-05

## Context

The application is one `main.go` (339 lines) with global `db` and `tmpl`
variables, a hand-rolled path router, SQL inline in the handlers, and templates
read from `templates/*.html` in the working directory at start-up. JS and CSS
are loaded from CDNs, Tailwind through its Play CDN, which Tailwind documents
as not for production.

The roadmap adds four storage backends, two auth modes, a JSON API, backups,
and several deployment targets. None of that fits in one file, and each of
them needs a seam that does not exist today: a storage interface, a service
layer the two front-ends share, and a binary that runs the same everywhere.

## Decision

1. Split the code into the packages described in
   [`docs/architecture.md`](../architecture.md): `cmd/kanban`, `assets`,
   `internal/{config,model,store,service,web}` now; `api`, `auth`, `backup`
   when their phase starts.
2. Keep the web layer on the standard library: `net/http` with Go 1.22 method
   patterns replaces `cardRouter`; `html/template` stays; `log/slog` replaces
   `log`.
3. Embed templates and static assets with `embed`. Third-party JS/CSS is
   vendored into `assets/` with a `VERSIONS` file naming version and licence.
   No runtime CDN.
4. Build with `CGO_ENABLED=0`. Backends must be pure Go (`modernc.org/sqlite`
   in Phase 1, not `mattn/go-sqlite3`). Final image is distroless, non-root.
5. `kanban` grows subcommands (`serve`, `migrate`, `version`). No arguments
   means `serve`, so existing deployments keep working.
6. Every new Go module dependency is justified in an ADR. Phase 0 adds none;
   `go mod tidy` removes one (`godotenv`, unused).

## Consequences

- Handlers become thin and testable without a database. The service layer is
  where behaviour lives and is the only thing the JSON API needs to reuse.
- The binary is relocatable and about 10 MB; `docker run` no longer needs
  the source tree or the templates directory in the image.
- Contributors need Go 1.24 only. No Node, no build step for the front-end.
- The Tailwind question is forced into the open: it can be vendored as the
  Play CDN script (works offline, ~400 kB, runtime JIT, officially dev-only),
  compiled once with the standalone `tailwindcss` binary under `go generate`
  with the CSS committed, or replaced with Bootstrap as the roadmap assumed.
  That decision is deferred to Phase 2 and listed in the roadmap's open
  decisions; Phase 0 carries the current markup over unchanged.
- The `/card/...` paths change. They are internal to the pages, so nothing
  outside the app depends on them.
