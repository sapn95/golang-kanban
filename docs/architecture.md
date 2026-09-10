# Architecture

This document describes the layout of the code base and the rules that keep it
that way. Everything this fork added to upstream's single `main.go` came in as a
package of its own: the storage backends, request identity, the JSON API,
snapshots and the deployment templates.

Decisions that are hard to reverse are recorded as ADRs in [`docs/adr/`](adr/).
Numbers in brackets below point at them.

## Ground rules

- **Standard library only for the web layer.** `net/http` with Go 1.22 method
  patterns for routing, `html/template` for rendering, `log/slog` for logs. No
  web framework, no ORM, no code generator that needs Node. The one build step
  the front-end has is the Tailwind compile, and it is a single pinned binary
  whose output is committed ([0011](adr/0011-tailwind-is-compiled.md)).
- **One static binary.** Templates and vendored JS/CSS are embedded with
  `embed`; the binary runs from any working directory. `CGO_ENABLED=0` so the
  image can be `scratch`/distroless and the same binary works on any distro.
- **Air-gapped by default.** Nothing is loaded from a CDN at runtime. Every
  third-party JS/CSS file lives in `assets/` with its version and licence.
- **Configuration is environment variables.** Existing variables keep their
  names and defaults. A config file, if it ever comes, is a thin layer that
  sets the same values.
- **Every new Go dependency gets an ADR.** There are two: `lib/pq`, which came
  from upstream, and `modernc.org/sqlite`
  ([0004](adr/0004-sqlite-backend.md)), which is pure Go because the
  `CGO_ENABLED=0` rule above outranks the faster cgo driver. Markdown, S3 signing
  and the JSON API are stdlib for the same reason.

## Package layout

```text
.
├── cmd/kanban/            main(): parses the subcommand, wires config → store → service → http
├── assets/                vendored htmx, SortableJS, icons; compiled tailwind.css [0011]; embed.go
├── internal/
│   ├── config/            Config struct, FromEnv(), Settings()/Redacted() for `kanban doctor`
│   ├── model/             Board, Column, Card, Label, Subtask, ID — plain structs, no deps  [0002]
│   │                      plus SLA and its office-hours clock, arithmetic on those structs [0013]
│   ├── store/             Store interface, sentinel errors, migration runner                 [0003]
│   │   ├── storetest/     contract test suite every backend must pass
│   │   ├── memory/        in-memory backend; tests and demos
│   │   ├── postgres/      lib/pq backend, embedded migrations/*.sql
│   │   └── sqlite/        modernc.org/sqlite backend, embedded migrations/*.sql [0004]
│   ├── service/           use cases: one method per user action, validation, WIP limits, IDs
│   ├── web/               HTMX handlers, embedded templates/, view models, markdown  [0010]
│   ├── api/               JSON handlers over the same service, under /api/v1  [0009]
│   ├── identity/          identity middleware: none | proxy | access          [0005]
│   └── backup/            snapshot Export/Import, directory and S3 targets, schedule [0012]
├── docs/                  plain markdown; configuration.md is generated, ADRs under docs/adr/
└── deploy/                helm/ chart, compose/ profiles, unraid/ container template
```

Everything above exists today; empty directories are not committed.

## Dependency direction

```text
cmd/kanban ──► web ──► service ──► store (interface) ◄── store/postgres
            ├► api ─┘      │         ▲                ◄── store/memory
            └► backup ───────────────┘                ◄── store/sqlite
                           ▼
                         model
```

- `model` imports nothing from this module. `store` imports `model`. Backends
  import `store` and `model`. `service` imports `store` and `model`. `web` and
  `api` import `service` and `model`, never a backend.
- `backup` is the one package that goes to `store` instead of to `service`: a
  snapshot is the rows as they are, and a restore has to write a card that
  today's validation rules would refuse. It is written once for every backend
  for the same reason ([0012](adr/0012-snapshots-are-the-portable-format.md)).
- Only `cmd/kanban` knows which backend is in use. It is the only place that
  imports `store/postgres`, `store/sqlite`, and so on. The same holds for the
  backup target: `cmd/kanban` builds the directory or the bucket, and `backup`
  only knows the `Target` interface.
- There is no `store/mongo` and no `store/s3`. A bucket cannot give the
  interface a transaction or a unique slug, so S3 is where snapshots go rather
  than where boards live; 0012 has the whole argument.
- Handlers do not run SQL and do not contain business rules. They parse the
  request, call one service method, and render the result. The JSON API calls
  the same methods; that is what stops the two front-ends from drifting apart.
- `web` mounts the JSON API as an `http.Handler` (`web.WithAPI`) and does not
  import `api`. Only `cmd/kanban` builds both.

## Request flow (HTMX)

```text
browser ──► web.Router (net/http mux, Go 1.22 patterns)
                │  parses path/form, resolves identity [0005]
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

The page paths are not versioned. They are the pages' own vocabulary rather
than a published API, so a rename is cheap inside a release and breaks anything
that calls a path directly, a script or a bookmark. `/b/{board}/labels` is the
one that has already happened, and it kept a `301`. Boards are first-class and
the hand-rolled `cardRouter` is gone in favour of method patterns.

The JSON API is versioned, at `/api/v1/`, because its callers are programs
nobody is going to fix by hand. What that prefix promises and what the two
surfaces share is [0009](adr/0009-json-api.md).

Every route, what it takes and what it answers is in
[`docs/api.md`](api.md), and a test compares that file against the
registrations in `internal/web/server.go`, so it says what the build does. The
shape worth knowing here:

- Reads are `GET`, writes are `POST`, and a write answers a fragment for htmx
  or a `303` for a plain form.
- A board is addressed by slug (`/b/{board}`), a card and a comment by id.
- `/assets/…` serves the embedded tree with a year-long cache, because the URL
  carries the digest.
- `/healthz` says the process is up, `/readyz` runs `store.Ping`, `/version`
  says which build is answering.
- `/manifest.webmanifest`, `/sw.js` and `/offline` are at the root rather than
  under `/assets/`, because a service worker controls only what is under the
  path it was served from and a manifest's scope defaults to its own directory.
  Both are served `no-cache`: the assets can be immutable for a year because
  their URL carries a digest, and these two have no digest to carry.

A drag between columns sends one `order` request for the destination column.
`ReorderCards` moves any listed card that currently lives in another column
into this one, so the origin column needs no second request and no
renumbering; gaps in `position` are harmless because ordering is by position,
not by contiguity.

The response is the board's column headers, each marked `hx-swap-oob`, and so
is the response to adding, archiving or deleting a card. The count, the WIP bar
and the notice under it are the limit's whole interface, and rendering them on
the server is what keeps "what does a full column look like" from being
answered a second time in JavaScript. The browser's part is now the drag itself
and the list of ids it posts.

## Command line

`kanban` with no arguments behaves like today (`serve`), so the Dockerfile `CMD`
and the deployment templates need no command of their own.

```text
kanban                 same as `kanban serve`
kanban serve           run the HTTP server
kanban migrate         apply pending migrations and exit
kanban version         print version, commit, Go version
kanban export          write a snapshot of every board to stdout or to -o file
kanban import          read one back: -replace overwrites, -dry-run only checks
kanban doctor          print the configuration and check what it points at
```

Subcommands use `flag` and a `switch`; no CLI library.

`doctor` is the one subcommand that runs before `config.FromEnv` can be trusted:
a configuration that does not validate is the thing it is there to print, so
`main` routes it ahead of the store as well, and `AUTO_MIGRATE` never turns a
diagnostic into a write. Every check runs whatever the ones before it found, so
one report shows all of it, and the exit code is 1 only on a failure. `none` as
an identity mode and `memory` as a store are choices somebody made, not faults,
and a diagnostic that exits non-zero on a working homelab is a diagnostic people
stop reading. The report walks the `env` and `secret` tags on `Config` by
reflection: tagging a new field is what puts it in the report, and a test asserts
that every tag names a variable `FromEnv` actually reads. Each probe says what it
did: a directory is checked by creating a file and removing it again, while a
bucket is only listed, and the line says a write was not attempted, because the
credentials that can list are often not the credentials that can put.

`export` and `import` speak one format across all three backends, which is what
makes them the way to move a board between them; the document and what it leaves
out is [0012](adr/0012-snapshots-are-the-portable-format.md). `serve` runs the
same export on a timer when a target is configured, writes it to a directory or
an S3 bucket, and deletes the ones past `BACKUP_KEEP`. The schedule lives in the
serving process because these deployments are one container, and an operator who
would rather drive it from outside sets `BACKUP_INTERVAL=0` and runs `kanban
export` on their own schedule.

## Configuration

Every variable, its default and what it does is in
[`docs/configuration.md`](configuration.md). That page is not written by hand:
`go generate ./internal/config/` renders it from `Config`, taking the names from
the `env` tags, the Notes column from the comment beside each field, the
paragraphs from the comment above each group, and the defaults from `FromEnv` with
an empty environment. A test renders it again and fails when the committed page
differs, so a variable cannot arrive without its row. The same tags drive the
`kanban doctor` report, which makes a new setting a tagged field and nothing
else.

`internal/model/zones.go` is generated the same way, from the Go toolchain's
`lib/time/zoneinfo.zip`, which is the database `time/tzdata` embeds. The
time-zone picker therefore offers exactly the names the server can load, and a
test walks the list and loads every one of them rather than trusting the
generator.

`config.FromEnv()` is the only place that reads the environment, and `Validate()`
refuses what cannot work before the server binds: two backup targets, a bucket
with no region or no credentials, a prefix that would need escaping in a signed
URL. The precedence rules worth knowing outside the table: `LISTEN_ADDR` wins over
`SERVER_PORT`, `DATABASE_URL` wins over the `DB_*` variables, `BACKUP_S3_REGION`
falls back to `AWS_REGION`, and `STORAGE` still defaults to `postgres` so an
upgrade keeps the database it had ([0004](adr/0004-sqlite-backend.md)).

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
- `/healthz` and `/readyz` as above. The chart's liveness probe takes `/healthz`
  and the readiness probe `/readyz`, so a pod whose database has gone away leaves
  the service instead of being restarted in a loop.
- `kanban doctor` for the questions a probe cannot answer: which variables
  arrived, whether the identity provider is reachable, whether snapshots are
  being written.
- Metrics are out of scope until someone asks.

## Testing

- `go test ./...` runs without any external service: `store/memory` and
  `store/sqlite` back the service and handler tests, the latter on a temporary
  file per subtest.
- `store/storetest.Run(t, newStore)` is the contract suite. Every backend runs
  it; Postgres runs it in CI against a service container and is skipped when
  `KANBAN_TEST_POSTGRES_URL` is unset.
- Handler tests use `httptest` against the real mux with the memory store.
- `internal/backup` proves the round trip across every pair of backends: export,
  import into another store, export again, and the two documents are compared
  byte for byte. A golden snapshot in `testdata/` catches the case a round trip
  cannot, which is Export and Import agreeing on something new.
- The S3 target signs its own requests, so its tests pin signatures botocore
  produced for the same requests, and the key derivation is checked against the
  `get-vanilla` vector AWS publishes. A test that only agreed with the code
  beside it would say nothing about whether a real bucket accepts the header.
- Documentation that describes an interface is tested against the interface:
  `docs/api.md` against the route registrations in `internal/web/server.go`,
  `docs/configuration.md` by rendering it again from `Config`, and the readme's
  environment block against the same list of variables. Each of those is a page
  somebody reads instead of the code, so being wrong is worse than being missing.

## Deployment

Three shapes, one image, and no special build for any of them:

- [`deploy/helm/kanban`](../deploy/helm/kanban) for Kubernetes: a read-only root
  filesystem, an optional bundled PostgreSQL, a network policy, and an
  oauth2-proxy sidecar that gives `AUTH_MODE=proxy` a header it is allowed to
  trust ([0005](adr/0005-request-identity.md)).
- [`deploy/compose`](../deploy/compose) for one machine: `sqlite`, `postgres` and
  `demo` profiles over a released tag. The `docker-compose.yml` in the root stays
  what it was, the developer's build from the working tree.
- [`deploy/unraid`](../deploy/unraid) for the Docker tab, in the shape Community
  Applications uses but installed by hand rather than published there. It carries
  `--user 99:100` because the image runs as uid 65532 and `/mnt/user/appdata`
  belongs to `nobody:users`.

The image has no shell, so `kanban doctor` is a subcommand rather than a recipe
in the readme: `docker exec kanban /kanban doctor` is the only way to ask a
running container what it read.

None of this can be unit-tested, so CI checks what it can. The chart renders
every values combination it claims to support and refuses the rest; all three
compose profiles interpolate with an empty environment and with `.env.example`,
because compose interpolates the whole file whichever profile is asked for; the
Unraid XML parses with the attributes the form needs; and one release version
appears in the chart, the compose file, `.env.example`, the template and the
readme, since a release that bumps four of the five is the mistake to expect.
Every environment variable those files name is compared against the `env` tags on
`Config` by a Go test, because a variable spelled wrong in a template is a
setting that does nothing at all.

## Build and CI

- The Dockerfile is multi-stage: an alpine `golang` builder runs
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`, and the binary ships in
  `gcr.io/distroless/static-debian13:nonroot` with `VOLUME /data` for the
  SQLite file. Every backend is pure Go, so the build stays CGO-free
  ([0004](adr/0004-sqlite-backend.md)).
- `ci.yml` has six jobs. `lint` runs gofmt, `go vet` and golangci-lint;
  `workflows` runs actionlint; `css` recompiles `assets/tailwind.css` and fails
  if it differs from what is committed; `test` runs `go test ./... -race
  -coverpkg=./...` against a Postgres service container and fails below 80%
  total coverage; `chart` lints the Helm chart and renders every values
  combination it claims to support, including the ones it has to refuse; and
  `deploy` validates the compose profiles and the Unraid template as above.
- Nothing pins a Go version twice. `go.mod` holds it and the workflows read it
  with `go-version-file`, so the only place that can drift is the builder image
  in the Dockerfile.
- `codeql.yml` analyses Go on every pull request and weekly, because a new
  query pack finds things in code that has not changed. `release.yml` builds
  and pushes the multi-arch image with a provenance attestation.
- Dependabot groups one pull request per ecosystem per week, and
  `dependabot-auto-merge.yml` merges it once every check on that commit is
  green. It reads its configuration from the default branch, which is why
  `homelab` is the default branch and `main`, which tracks upstream, is not.
- `godotenv` is gone from `go.mod`; it was required but never imported.

## What the restructure does not change

- Postgres users keep their connection variables. Their `cards` table is
  migrated, not abandoned; see [0002](adr/0002-board-column-card-model.md) for
  exactly what happens to it.
- The look of the board. Templates moved and got a layout file, but the markup
  and the dark-mode toggle are carried over as they were. What did change is how
  Tailwind gets there: it is compiled from those templates into
  `assets/tailwind.css` instead of working the classes out in the browser
  ([0011](adr/0011-tailwind-is-compiled.md)).
- The port and the shape of the `docker run` one-liner. The image name in it
  does change: this fork builds its own package, `ghcr.io/sapn95/golang-kanban`,
  because upstream's stops at 1.0.1.
