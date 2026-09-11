# 0004 — SQLite on a pure-Go driver

Status: accepted
Date: 2026-09-06

## Context

Postgres is the only backend that survives a restart. On the install this is
actually run on, a Raspberry Pi with three people sharing one board, that means
a second container, a volume, a healthcheck, a set of credentials and a
major-version upgrade every year, in front of a database holding a few hundred
rows. The roadmap has listed SQLite as the next backend since Phase 1, and the
readme's todo list has it as "for people too lazy to setup a db".

[0001](0001-package-layout-and-single-static-binary.md) already names
`modernc.org/sqlite`, and in the same breath requires that every new dependency
is justified in an ADR. This is the first one added under that rule, so it gets
the argument rather than the mention. The constraint that settles it is in that
ADR too: the binary is built with `CGO_ENABLED=0` and shipped in
`gcr.io/distroless/static-debian12`.

## Decision

### The backend

`internal/store/sqlite` implements `store.Store`
([0003](0003-store-interface-and-portable-ids.md)) and passes `storetest`. It
is selected with `STORAGE=sqlite`; the file is `SQLITE_PATH`, default
`/data/kanban.db`, and the image already declares `VOLUME ["/data"]`, so
mounting a directory there is the whole of the setup. The schema is the SQLite
dialect from [0002](0002-board-column-card-model.md), and the v1 `cards` import
runs the way it does on Postgres.

`STORAGE` keeps defaulting to `postgres`. Flipping the default would point an
existing `docker run` at an empty file instead of at its database and report a
successful start, which is worse than any error. The readme sends new installs
to `sqlite`; old ones change nothing.

### The driver

`modernc.org/sqlite`, the C amalgamation machine-translated into Go, not
`github.com/mattn/go-sqlite3`.

mattn is the better engine, because it is SQLite: the upstream C, tested by
upstream, faster than a translation of itself. It is also cgo, and cgo costs
the property this project is built around. `CGO_ENABLED=1` links a libc, which
rules out `distroless/static` and `scratch`. Building the arm64 image needs a C
cross-toolchain in CI instead of `GOARCH=arm64`. `go test ./...` needs a C
compiler on every contributor's machine. The rule in 0001 would turn into
per-backend build tags, and the one artefact this project promises, a static
binary that runs anywhere, would depend on which backend was compiled in.

Alternatives considered:

- `zombiezen.com/go/sqlite`: the same translated engine under its own API. The
  migration runner, every other backend and every test here speak
  `database/sql`, and a second idiom for one backend buys nothing.
- Out-of-process SQLite (`sqinn-go`): trades cgo for a second executable to
  ship and update, which moves the dependency rather than removing it.

### What it costs

- About 3.5 MB of binary and eleven modules, one direct and ten indirect, for a
  translated libc that one backend uses. Measured on linux/arm64 with
  `-trimpath -ldflags="-s -w"`.
- Translated C is slower than C. A board with a few hundred cards never finds
  out, and if some later install does, the store interface makes a cgo backend
  a sibling package behind a build tag rather than a rewrite.
- The engine is not the code path upstream tests, so a translation bug is our
  bug. `storetest` against a real file on every `go test ./...` is the only
  answer we have to that, which is why the suite runs there and not only
  against Postgres.

### One writer at a time, one node

SQLite serialises writers, and the backend does not hide it. The pool is capped
at one connection, so writers queue inside `database/sql`, where waiting is
free and bounded by the request context, instead of spinning on `busy_timeout`
and failing with "database is locked". WAL keeps readers running while a write
holds the file.

This makes the backend one for a single process on a single machine with a
local disk. Two replicas over a shared volume, or a database file on NFS, is
not supported and will not become supported; that case is what Postgres is
still here for.

## Consequences

- The install with no moving parts is
  `docker run -v kanban:/data -e STORAGE=sqlite`. The Postgres service in
  compose becomes a choice rather than a prerequisite.
- `go test ./...` exercises a real SQL backend without a service container, on
  a temporary file per subtest. Postgres stays in CI for what is Postgres.
- The database is a file plus a WAL, so a backup is `kanban export` (Phase 5)
  or SQLite's own backup, never `cp` while the process is running.
- The dialect in 0002 is now fixed in a schema: timestamps are RFC 3339 text
  with a fixed-width fraction so that they sort as instants, dates are
  `YYYY-MM-DD`, booleans are 0 and 1. Later migrations keep those formats or
  break ordering.
- Mongo and S3 (Phase 1) are unaffected. They answer other questions than
  "smaller", and this backend takes none of their arguments away.
