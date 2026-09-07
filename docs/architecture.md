# Architecture

This document describes the target layout of the code base and the rules that
keep it that way. It is the reference for the restructuring in Phase 0 of the
roadmap; every later phase (storage backends, auth, JSON API, backup, deploy)
adds a package here rather than a file to `main.go`.

Decisions that are hard to reverse are recorded as ADRs in [`docs/adr/`](adr/).
Numbers in brackets below point at them.

## Ground rules

- **Standard library only for the web layer.** `net/http` with Go 1.22 method
  patterns for routing, `html/template` for rendering, `log/slog` for logs. No
  web framework, no ORM, no code generator that needs Node.
- **One static binary.** Templates and vendored JS/CSS are embedded with
  `embed`; the binary runs from any working directory. `CGO_ENABLED=0` so the
  image can be `scratch`/distroless and the same binary works on any distro.
- **Air-gapped by default.** Nothing is loaded from a CDN at runtime. Every
  third-party JS/CSS file lives in `assets/` with its version and licence.
- **Configuration is environment variables.** Existing variables keep their
  names and defaults. A config file, if it ever comes, is a thin layer that
  sets the same values.
- **Every new Go dependency gets an ADR.** Phase 0 adds none; Phase 1 adds
  `modernc.org/sqlite` ([0004](adr/0004-sqlite-backend.md)), which is pure Go
  because the `CGO_ENABLED=0` rule above outranks the faster cgo driver.

## Package layout

```text
.
├── cmd/kanban/            main(): parses the subcommand, wires config → store → service → http
├── assets/                vendored htmx, SortableJS, icons, CSS; package `assets` with embed.go
├── internal/
│   ├── config/            Config struct, FromEnv(), Redacted() for `kanban doctor` later
│   ├── model/             Board, Column, Card, Label, Subtask, ID — plain structs, no deps  [0002]
│   ├── store/             Store interface, sentinel errors, migration runner                 [0003]
│   │   ├── storetest/     contract test suite every backend must pass
│   │   ├── memory/        in-memory backend; tests and demos
│   │   ├── postgres/      lib/pq backend, embedded migrations/*.sql
│   │   └── sqlite/        modernc.org/sqlite backend, embedded migrations/*.sql [0004]
│   ├── service/           use cases: one method per user action, validation, WIP limits, IDs
│   ├── web/               HTMX handlers, embedded templates/, view models
│   ├── api/               (Phase 4) JSON handlers over the same service
│   ├── auth/              (Phase 3) identity middleware: none | proxy | oidc
│   └── backup/            (Phase 5) Export/Import snapshot, S3 scheduler
├── docs/                  plain markdown, ADRs under docs/adr/
└── deploy/                (Phase 6) compose profiles, haproxy, helm, terraform, unraid
```

The packages Phase 0 needs exist today, plus the SQLite backend (`cmd/kanban`,
`assets`, `config`, `model`, `store` + `memory` + `postgres` + `sqlite` +
`storetest`, `service`, `web`). The others are listed so their place is agreed
now; empty directories are not committed.

## Dependency direction

```text
cmd/kanban ──► web ──► service ──► store (interface) ◄── store/postgres
            └► api ─┘      │                          ◄── store/memory
                           ▼                          ◄── store/sqlite
                         model  ◄──────────────────────── store/mongo    (Phase 1)
                                                      ◄── store/s3       (Phase 1)
```

- `model` imports nothing from this module. `store` imports `model`. Backends
  import `store` and `model`. `service` imports `store` and `model`. `web` and
  `api` import `service` and `model`, never a backend.
- Only `cmd/kanban` knows which backend is in use. It is the only place that
  imports `store/postgres`, `store/sqlite`, and so on.
- Handlers do not run SQL and do not contain business rules. They parse the
  request, call one service method, and render the result. The JSON API in
  Phase 4 calls the same methods; that is what stops the two front-ends from
  drifting apart.

## Request flow (HTMX)

```text
browser ──► web.Router (net/http mux, Go 1.22 patterns)
                │  parses path/form, resolves identity (Phase 3)
                ▼
            service.Kanban.MoveCard(ctx, cardID, columnID, index)
                │  validates, checks WIP limit, stamps updated_at
                ▼
            store.Store.ReorderCards(ctx, boardID, columnID, order)   ── one atomic operation
                │
                ▼
            web renders card.html / board.html fragment ──► browser swaps it in
```

## URL scheme

The current handlers are internal to the HTMX pages, so the paths can change
without breaking anyone. Boards become first-class, and the hand-rolled
`cardRouter` goes away in favour of method patterns.

| Method | Path                                   | Purpose                                              |
|--------|----------------------------------------|------------------------------------------------------|
| GET    | `/`                                    | redirect to the only board, or list boards           |
| GET    | `/b/{board}`                           | render a board (`{board}` is the slug)               |
| POST   | `/b/{board}/cards`                     | create a card in a column (form field `column`)      |
| GET    | `/cards/{id}`                          | card fragment (re-render after edit)                 |
| GET    | `/cards/{id}/edit`                     | edit form fragment                                   |
| POST   | `/cards/{id}`                          | update title/description/due date/labels/subtasks    |
| POST   | `/cards/{id}/assignee`                 | quick edit: form field `assignee`, `@me` or empty    |
| POST   | `/cards/{id}/labels/{label}/toggle`    | quick edit: put one of the board's labels on or off  |
| POST   | `/cards/{id}/delete`                   | delete                                               |
| POST   | `/b/{board}/columns/{column}/order`    | JSON `{"order":[ids]}`: authoritative card order     |
| POST   | `/b/{board}/columns`                   | (Phase 2) create column                              |
| POST   | `/b/{board}/columns/{column}`          | (Phase 2) rename / WIP limit                         |
| POST   | `/b/{board}/columns/order`             | (Phase 2) reorder columns                            |
| GET    | `/assets/…`                            | embedded static files, long cache, content-hashed    |
| GET    | `/healthz`                             | process is up; always 200                            |
| GET    | `/readyz`                              | `store.Ping` succeeds; 503 otherwise                 |

A drag between columns sends one `order` request for the destination column.
`ReorderCards` moves any listed card that currently lives in another column
into this one, so the origin column needs no second request and no
renumbering; gaps in `position` are harmless because ordering is by position,
not by contiguity.

## Command line

`kanban` with no arguments behaves like today (`serve`), so the existing
Dockerfile `CMD` and Unraid template keep working.

```text
kanban                 same as `kanban serve`
kanban serve           run the HTTP server
kanban migrate         apply pending migrations and exit
kanban version         print version, commit, Go version
kanban export|import   (Phase 5)
kanban doctor          (Phase 6)
```

Subcommands use `flag` and a `switch`; no CLI library.

## Configuration (Phase 0)

| Variable        | Default     | Notes                                                      |
|-----------------|-------------|------------------------------------------------------------|
| `SERVER_PORT`   | `17808`     | unchanged                                                  |
| `LISTEN_ADDR`   | `:$SERVER_PORT` | new; wins over `SERVER_PORT` when set                  |
| `STORAGE`       | `postgres`  | `postgres`, `sqlite` or `memory`; the default stays `postgres` so upgrades keep their database ([0004](adr/0004-sqlite-backend.md)) |
| `DATABASE_URL`  | *(unset)*   | new; full DSN, wins over the `DB_*` variables              |
| `DB_USER` `DB_PASS` `DB_HOST` `DB_PORT` `DB_NAME` | as today | unchanged                              |
| `DB_SSLMODE`    | `disable`   | new; today's hard-coded value becomes the default          |
| `SQLITE_PATH`   | `/data/kanban.db` | `STORAGE=sqlite` only; the image declares `VOLUME ["/data"]` |
| `AUTO_MIGRATE`  | `true`      | run migrations on `serve` start; `false` to require `kanban migrate` |
| `LOG_LEVEL`     | `info`      | `debug` `info` `warn` `error`                              |
| `LOG_FORMAT`    | `text`      | `text` or `json`                                           |

`config.FromEnv()` is the only place that reads the environment. The reference
table in `docs/configuration.md` is generated from the struct tags by
`go generate` (Phase 7), so the table above is the hand-written stopgap.

## Migrations

Each SQL backend embeds its own `migrations/NNNN_name.sql`; the runner in
`internal/store` is ~60 lines of stdlib: a `schema_migrations(version)` table,
each file applied in one transaction. A migration may also be a Go function
for steps that are easier in code than in SQL; the v1 data migration is one
(see [0002](adr/0002-board-column-card-model.md)), and so is the v1 table
rename on SQLite, which has no conditional DDL. No migration library.

`AUTO_MIGRATE=true` on start keeps `docker run` zero-setup. Operators who want
control run `kanban migrate` in a job and set `AUTO_MIGRATE=false`.

## Observability

- `log/slog`, one logger created in `cmd/kanban`, passed down. Request log
  line per request: method, path, status, duration, remote IP.
- `/healthz` and `/readyz` as above; HAProxy (Phase 3) checks `/readyz`.
- Metrics are out of scope until someone asks.

## Testing

- `go test ./...` runs without any external service: `store/memory` and
  `store/sqlite` back the service and handler tests, the latter on a temporary
  file per subtest.
- `store/storetest.Run(t, newStore)` is the contract suite. Every backend runs
  it; Postgres runs it in CI against a service container and is skipped when
  `KANBAN_TEST_POSTGRES_URL` is unset.
- Handler tests use `httptest` against the real mux with the memory store.

## Build and CI

- Dockerfile becomes multi-stage: `golang:1.24` builder with
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`, final image
  `gcr.io/distroless/static`, non-root, `VOLUME /data` (the SQLite file).
  Every backend is pure Go, so the build stays CGO-free
  ([0004](adr/0004-sqlite-backend.md)).
- `lint.yml` reads the Go version from `go.mod` (`go-version-file`) instead of
  pinning 1.21 while `go.mod` says 1.24; a `test` job runs `go test ./...`.
- `test.yml` runs `go test ./... -race -coverpkg=./...` against a Postgres
  service container and fails below 80% total coverage.
- `godotenv` is gone from `go.mod`; it was required but never imported.

## What the restructure does not change

- Postgres users keep their connection variables. Their `cards` table is
  migrated, not abandoned; see [0002](adr/0002-board-column-card-model.md) for
  exactly what happens to it.
- The look of the board. Templates moved and got a layout file, but the markup
  and the dark-mode toggle are carried over as they were. Tailwind is served as
  the vendored Play build for now (`assets/VERSIONS`); whether to keep that,
  compile once with the standalone CLI, or move to Bootstrap is an open
  decision for the UX phase.
- The port, the Docker image name, the `docker run` one-liner.
