## Simple Kanban Board
A no-nonsense, lightweight Kanban board built with Golang and HTMX. I couldn’t find a reasonable self-hosted Kanban board that wasn’t using some JavaScript monstrosity like Node or Next.js, I don't need the next [9.1 CVE](https://github.com/advisories/GHSA-f82v-jwr5-mffw) on my server — so I decided to build my own. This project is my sweet little solution to manage tasks simply while learning and sharing a project with the community.

### Overview
Backend: Go, standard library `net/http`, one static binary with everything embedded.

Frontend: HTMX, Tailwind, SortableJS, vendored, so nothing is loaded from a CDN and it works air-gapped. Tailwind is compiled from the templates into a 38 kB stylesheet instead of working the classes out in the browser, which took 407 kB of JavaScript off every page load; see [docs/adr/0011](docs/adr/0011-tailwind-is-compiled.md).

Database: SQLite or PostgreSQL. SQLite is a file, needs nothing installed, and is meant for one board on one machine; PostgreSQL is there when you want more than that. The SQLite driver is pure Go, so the binary stays static and cgo-free — the reasoning is in [docs/adr/0004](docs/adr/0004-sqlite-backend.md).

### Features
- Boards with as many columns as you like, each with an optional WIP limit.
  Columns and labels are edited from the board's own settings page: add,
  rename, reorder, set a limit, and choose where a deleted column's cards go.
- Cards with description, due date, labels and a subtask checklist. The card
  face shows checklist progress, and grades a due date rather than only
  marking it late. A double click opens the card for editing, and a double
  click on the date itself opens the browser's own calendar instead, so a date
  moves without opening anything.
- The description is markdown, rendered by the server: bold, italic, lists,
  quotes, headings, `code`, fenced blocks and links. It is a documented subset
  and no dependency, and a link is only a link if its scheme is one of four;
  see [docs/adr/0010](docs/adr/0010-markdown-descriptions.md).
- Assignee per card, shown on the card face, with one click to take it yourself.
  With `AVATARS` set, the bubble shows the person's GitHub picture, fetched by
  the server and served from your own origin so that GitHub never sees who is
  looking at the board; see [docs/adr/0008](docs/adr/0008-avatars-are-proxied.md).
- Archive a card instead of deleting it: it leaves the board, keeps its column
  and labels, and restoring puts back the same card. The archive has a search
  box of its own, on the same syntax as the board's, so finding the one card
  somebody archived in March does not mean scrolling to March.
- Comments on a card, with who wrote them and when. They cannot be edited and
  the author can remove their own; the reasoning is in
  [docs/adr/0007](docs/adr/0007-comments-are-append-only.md).
- A response time, for a board that is somebody's support desk: how many hours a
  card may sit untouched, which days the desk is open, between which hours and in
  which zone. The card face carries what is left of it, and the count is office
  hours, so a card that arrives on Friday evening is not overdue on Saturday
  morning. Touching the card starts it again, and a column can be marked as one
  where the clock does not run, such as Done or Waiting for the customer; see
  [docs/adr/0013](docs/adr/0013-response-time-is-office-hours.md).
- Search on the board's own URL, so a result set can be linked to. A bare
  word matches the title, the description or a label name:
  `label:bug assignee:someone due:overdue is:archived`, plus `"quoted phrases"`.
  A label on a card is a link to that search, so the syntax is discoverable
  rather than something you have to know about, and the chip carries an `x`
  that takes the label off the card. A word that is nowhere to be found as it
  was typed is tried again as a typo, so `logni` finds the login card. Under
  four letters a word is matched exactly, and a quoted phrase always is.
- Select several cards (shift-click for a range) and move, assign, archive or
  delete them at once. Dragging one card of a selection takes the whole
  selection with it.
- Drag-and-drop between columns with SortableJS; partial updates with HTMX.
- A JSON API under `/api/v1/` for the things a page is bad at: a card from a
  cron job, a count over a shell pipeline, a fresh install seeded with its
  columns. It calls the same code the pages call, and the contract is
  [`openapi.json`](internal/api/openapi.json), served at
  `/api/v1/openapi.json`; see [docs/adr/0009](docs/adr/0009-json-api.md).
- `kanban export` writes every board to one JSON file and `kanban import` reads
  it back, on any backend, so a board moves from the SQLite file on your laptop
  into the PostgreSQL in your cluster without SQL. With `BACKUP_DIR` or
  `BACKUP_S3_BUCKET` set, the server writes the same file on a timer and keeps
  the last `BACKUP_KEEP` of them; see
  [docs/adr/0012](docs/adr/0012-snapshots-are-the-portable-format.md).
- Dark mode.
- Cross-site writes are refused, security headers are set, and request bodies
  are capped; see [docs/adr/0006](docs/adr/0006-cross-site-writes.md).
- Schema migrations run on start; an existing single-table database is imported automatically.

### How it looks
![A board with three columns: a WIP limit at its limit, label chips, graded due dates, checklist progress and a markdown description](docs/img/screenshot.png)

<details>
<summary>The same board in dark mode</summary>

![The same board with the dark palette](docs/img/screenshot-dark.png)

</details>

### Using Docker Compose
The [`docker-compose.yml`](docker-compose.yml) in the repository root builds the
image from the working tree, which is what you want while changing the code:
`docker compose up --build` starts PostgreSQL and the board on
http://localhost:17808.

To run a released image instead, [`deploy/compose`](deploy/compose) has three
profiles: `sqlite` is a file on a volume with nothing to install, `postgres`
brings a database container as well, and `demo` keeps the board in memory and
throws it away on exit.

``` bash
cd deploy/compose
cp .env.example .env
docker compose --profile sqlite up -d
```

### Using the Pre-built Docker Image
The image is built for linux/amd64 and linux/arm64 with a provenance
attestation, so it runs on a laptop and on a Raspberry Pi from the same tag.

``` bash
docker pull ghcr.io/sapn95/golang-kanban:2.4.0

# SQLite: one volume, no database to set up
docker run -p 17808:17808 -e STORAGE=sqlite -v kanban:/data ghcr.io/sapn95/golang-kanban:2.4.0

# PostgreSQL
docker run -p 17808:17808 -e DB_HOST=your-postgres -e DB_USER=... -e DB_PASS=... ghcr.io/sapn95/golang-kanban:2.4.0
```

### On Unraid
[`deploy/unraid/kanban.xml`](deploy/unraid/kanban.xml) is a template for the
Docker tab. It is not in Community Applications, so install it by hand: paste its
raw URL into the Template field, or copy the file to
`/boot/config/plugins/dockerMan/templates-user/`. The form then comes up with the
port, `/data` on appdata, SQLite and a daily snapshot already filled in. The
[notes beside it](deploy/unraid/README.md) cover PostgreSQL, snapshots and
sign-in.

### In Kubernetes
[`deploy/helm/kanban`](deploy/helm/kanban) is a chart with a read-only root
filesystem, an optional PostgreSQL, a network policy, and an oauth2-proxy sidecar
that puts Entra, GitHub or any OIDC provider in front of the board.

### Prerequisites
With `STORAGE=sqlite` there are none: point `SQLITE_PATH` at a file in a writable directory (`/data` in the image) and the tables are created on first start. One process on one machine, and writes are serialised, which is plenty for a small team; see [docs/adr/0004](docs/adr/0004-sqlite-backend.md) for what that rules out.

PostgreSQL is the external dependency if you pick it. Create a database and a user that owns it; the tables are created by the app on first start (or with `kanban migrate`).

``` sql
CREATE USER kanban WITH PASSWORD 'your_password_here';
CREATE DATABASE kanban OWNER kanban;
```

Upgrading from a version that used the single `cards` table? Take a `pg_dump` first. The old table is imported into a default board on the first start and then dropped.

### Environment Variables
Every variable, its default and what it does is in
[docs/configuration.md](docs/configuration.md). That page is generated from the
struct that reads them, so it cannot fall behind the code. The ones a deployment
usually sets:

``` bash
SERVER_PORT=17808          # or LISTEN_ADDR=0.0.0.0:17808
STORAGE=postgres           # postgres | sqlite | memory (memory: nothing is saved, handy for a demo)
SQLITE_PATH=/data/kanban.db # STORAGE=sqlite only; the image has a volume at /data
DATABASE_URL=              # full DSN; wins over the DB_* variables below
DB_USER=user
DB_PASS=password
DB_HOST=postgres
DB_PORT=5432
DB_NAME=kanban
DB_SSLMODE=disable
AUTO_MIGRATE=true          # false: run `kanban migrate` yourself
LOG_LEVEL=info             # debug | info | warn | error
LOG_FORMAT=text            # text | json

# Who is making the request. See docs/adr/0005 for why proxy and access
# are not interchangeable.
AUTH_MODE=none             # none | proxy | access
AUTH_HEADER=X-Forwarded-Email  # AUTH_MODE=proxy only
ACCESS_TEAM_DOMAIN=        # AUTH_MODE=access only, e.g. team.cloudflareaccess.com
ACCESS_AUD=                # AUTH_MODE=access only, the application's AUD tag
AUTH_REQUIRED=false        # true: refuse a request that arrives with no identity

# Who has a picture instead of initials, as address=github-login pairs. Unset
# means initials and no outbound request. A GitHub noreply address carries the
# login after the plus sign.
AVATARS=me@example.com=octocat,1+other@users.noreply.github.com=other

# Scheduled snapshots. Set a directory or a bucket, not both; with neither, the
# server takes no backups and `kanban export` is how you get one.
BACKUP_INTERVAL=24h        # 0 turns the schedule off; under a minute is refused
BACKUP_KEEP=7              # older snapshots are deleted after a successful write; 0 keeps all
BACKUP_DIR=                # e.g. /data/snapshots, on a volume that outlives the container
BACKUP_S3_BUCKET=          # the other target
BACKUP_S3_PREFIX=          # e.g. pi/, so one bucket can hold several boards
BACKUP_S3_REGION=          # or AWS_REGION; required with a bucket, it is part of the signature
BACKUP_S3_ENDPOINT=        # empty for AWS; https://minio.example.com:9000 for MinIO and the rest
AWS_ACCESS_KEY_ID=         # required with a bucket: there is no ~/.aws, no instance metadata
AWS_SECRET_ACCESS_KEY=
AWS_SESSION_TOKEN=         # only for temporary credentials
```

`kanban` with no arguments serves; `kanban migrate` applies migrations and exits; `kanban doctor` reports the configuration, below; `kanban version` prints the version; `kanban export` and `kanban import` are further down. `/healthz` says the process is up, `/readyz` says the database answers, and `/version` says which build is answering — which is how you find out whether a deploy actually landed, without fetching a page and looking for markup only the new version renders.

`AUTH_MODE` says how a caller is recognised. It does not say that a caller has
to be anybody: a request that arrives without the header or the assertion is
served anonymously, with every write the board has. That is fine where the port
is only reachable through the proxy, and it is a hole where it is not, which is
any deployment that also publishes a LAN port beside the tunnel. `AUTH_REQUIRED=true`
closes it: anything with no identity gets a `403`, except `/healthz` and
`/readyz`, which a kubelet has to be able to reach. `/version` goes behind it
with everything else, so the way to read a build off a running deployment
becomes the footer on the board or `kanban version` in the container. It needs
`AUTH_MODE=proxy` or `access`, and the trade is that a key rotation the app
cannot follow now refuses the page rather than drawing it signed out.

### When it does not come up

`kanban doctor` prints the configuration the process actually read, with the
passwords masked, and then tries the things a deployment gets wrong: the store,
the schema, the identity mode, the avatar pairs and the backup target. It writes
nothing, so it is safe against a running deployment.

``` bash
docker exec kanban /kanban doctor      # there is no shell in the image
```

``` text
settings
  SERVER_PORT            17808
  STORAGE                sqlite
  DB_PASS                [set]
  ...

derived
  listen address         :17808
  store                  sqlite /data/kanban.db

checks
  ok    configuration  29 variables, all valid
  ok    storage        sqlite /data/kanban.db answered in 4ms
  ok    schema         readable, 1 board: board
  ok    identity       proxy mode, reading X-Forwarded-Email; the port must not be reachable except through the proxy that sets it
  --    avatars        AVATARS is unset: the board draws initials and makes no outbound request
  ok    backup         dir /data/snapshots, every 24h0m0s, keeping 7, 1 snapshot there, newest kanban-20260909T161209Z.json (2h ago)
```

Every check runs whatever the ones before it found, so one report shows
everything that is wrong. `AUTH_MODE=none` and `STORAGE=memory` are choices, not
faults, so the exit code is 1 only when something failed: a store that will not
open, a schema that is not there, an identity provider that cannot be reached, a
backup target that refuses.

### The JSON API

``` bash
api=localhost:17808/api/v1
b=$(curl -s $api/boards | jq -r '.[0].slug')       # "board" on a fresh install
col=$(curl -s $api/boards/$b | jq -r '.columns[0].id')
curl -s -X POST $api/boards/$b/cards -H 'Content-Type: application/json' \
  -d "{\"title\":\"Backup ran\",\"column_id\":\"$col\",\"due_date\":\"2026-10-01\"}"
curl -s "$api/boards/$b/cards?q=label:bug" | jq length
curl -s $api/openapi.json                          # the whole contract
```

There are no tokens: the API grants what the board grants, so whatever protects
the pages protects it too. `@me` as an assignee resolves to whoever the request
is signed in as and is a `403` when nobody is. An unknown field in a body is a
`400` that names the field, because a script that says `titel` should hear
about it before the card exists.

### Backups and moving a board

``` bash
kanban export -o board.json          # every board, one JSON file, mode 600
kanban export | gzip > board.json.gz # or to stdout, so it can be piped

kanban import -dry-run board.json    # says what the file holds, writes nothing
kanban import board.json             # refuses a board that is already there
kanban import -replace board.json    # deletes those boards first, then writes
```

The file is the same on all three backends, so exporting from SQLite and
importing into PostgreSQL is the supported way to move a board between them.
IDs come back as they were, which means a link to a card survives the trip. What
the file does not carry, and what a restored archived card does, is in
[docs/adr/0012](docs/adr/0012-snapshots-are-the-portable-format.md).

With a target set, the server writes the same document on a timer and prunes to
`BACKUP_KEEP`, no cron job and no sidecar:

``` bash
docker run -p 17808:17808 -v kanban:/data \
  -e STORAGE=sqlite -e BACKUP_DIR=/data/snapshots -e BACKUP_INTERVAL=6h \
  ghcr.io/sapn95/golang-kanban:2.4.0
```

For a bucket, set `BACKUP_S3_BUCKET`, a region and the two AWS keys;
`BACKUP_S3_ENDPOINT` points the same code at MinIO, Garage or Backblaze B2. The
requests are signed by about a hundred lines in `internal/backup/s3.go` instead
of by the forty-module AWS SDK, and the signature is pinned in the tests against
what botocore produces for the same request. The file is every card, every
comment and every assignee's address, so a bucket that allows public reads
publishes the board.

### Building from source
``` bash
go build ./cmd/kanban
go test ./...                       # memory and sqlite backends, no database needed
KANBAN_TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/kanban_test?sslmode=disable go test ./...
```

Two things are generated and committed. Adding a Tailwind class to a template or
to `app.js` needs the stylesheet rebuilt, which fetches one pinned binary and no
package manager; adding an environment variable to `config.Config` needs the
reference page rendered again from it.

``` bash
go generate ./assets/               # writes assets/tailwind.css; commit it
go generate ./internal/config/      # writes docs/configuration.md; commit it
```

Both are checked in CI against what is committed, so forgetting one is a failing
build rather than a stale file.

#### Todo's
If I feel like it I might work on some of these things:
- [x] darkmode
- [x] remove/add/edit collums
- [ ] make it pretty
- [x] add sqlite option for people too lazy to setup a db
- [x] tls, terminated in front of the app by the ingress or by the
  oauth2-proxy sidecar in [deploy/helm](deploy/helm/kanban)
- [x] oidc, in the same place: Entra, GitHub or any OIDC provider signs people
  in, and the app reads who they are from the header the proxy sets
- [x] backups: one JSON snapshot of every board, written on a timer to a
  directory or an S3 bucket, and read back with `kanban import`
- [ ] ...

#### Contributing
The code layout and the rules it follows are in [docs/architecture.md](docs/architecture.md); every route with what it takes and what it answers is in [docs/api.md](docs/api.md); every setting is in [docs/configuration.md](docs/configuration.md); decisions are recorded in [docs/adr/](docs/adr/).

Contributions are welcome! If you have ideas, bug fixes, or enhancements, feel free to fork the repository, open an issue, or submit a pull request.

#### License
This project is open-sourced under the MIT License.
