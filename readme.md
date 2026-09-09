## Simple Kanban Board
A no-nonsense, lightweight Kanban board built with Golang and HTMX. I couldn’t find a reasonable self-hosted Kanban board that wasn’t using some JavaScript monstrosity like Node or Next.js, I don't need the next [9.1 CVE](https://github.com/advisories/GHSA-f82v-jwr5-mffw) on my server — so I decided to build my own. This project is my sweet little solution to manage tasks simply while learning and sharing a project with the community.

### Overview
Backend: Go, standard library `net/http`, one static binary with everything embedded.

Frontend: HTMX, Tailwind, SortableJS — vendored, nothing is loaded from a CDN, so it works air-gapped.

Database: SQLite or PostgreSQL. SQLite is a file, needs nothing installed, and is meant for one board on one machine; PostgreSQL is there when you want more than that. The SQLite driver is pure Go, so the binary stays static and cgo-free — the reasoning is in [docs/adr/0004](docs/adr/0004-sqlite-backend.md).

### Features
- Boards with as many columns as you like, each with an optional WIP limit.
  Columns and labels are edited from the board's own settings page: add,
  rename, reorder, set a limit, and choose where a deleted column's cards go.
- Cards with description, due date, labels and a subtask checklist. The card
  face shows checklist progress, and grades a due date rather than only
  marking it late.
- Assignee per card, shown on the card face, with one click to take it yourself.
  With `AVATARS` set, the bubble shows the person's GitHub picture, fetched by
  the server and served from your own origin so that GitHub never sees who is
  looking at the board; see [docs/adr/0008](docs/adr/0008-avatars-are-proxied.md).
- Archive a card instead of deleting it: it leaves the board, keeps its column
  and labels, and restoring puts back the same card.
- Comments on a card, with who wrote them and when. They cannot be edited and
  the author can remove their own; the reasoning is in
  [docs/adr/0007](docs/adr/0007-comments-are-append-only.md).
- Search on the board's own URL, so a result set can be linked to. A bare
  word matches the title, the description or a label name:
  `label:bug assignee:someone due:overdue is:archived`, plus `"quoted phrases"`.
  A label on a card is a link to that search, so the syntax is discoverable
  rather than something you have to know about, and the chip carries an `x`
  that takes the label off the card.
- Select several cards (shift-click for a range) and move, assign, archive or
  delete them at once. Dragging one card of a selection takes the whole
  selection with it.
- Drag-and-drop between columns with SortableJS; partial updates with HTMX.
- A JSON API under `/api/v1/` for the things a page is bad at: a card from a
  cron job, a count over a shell pipeline, a fresh install seeded with its
  columns. It calls the same code the pages call, and the contract is
  [`openapi.json`](internal/api/openapi.json), served at
  `/api/v1/openapi.json`; see [docs/adr/0009](docs/adr/0009-json-api.md).
- Dark mode.
- Cross-site writes are refused, security headers are set, and request bodies
  are capped; see [docs/adr/0006](docs/adr/0006-cross-site-writes.md).
- Schema migrations run on start; an existing single-table database is imported automatically.

### How it looks
![Screenshot](docs/img/screenshot-v1.0.0.png "Screenshot")

### Using Docker Compose
A sample docker-compose.yml is provided, just use `docker compose up --build`.
This starts PostgreSQL and the board on http://localhost:17808.

### Using the Pre-built Docker Image
The image is built for linux/amd64 and linux/arm64 with a provenance
attestation, so it runs on a laptop and on a Raspberry Pi from the same tag.

``` bash
docker pull ghcr.io/sapn95/golang-kanban:2.0.0

# SQLite: one volume, no database to set up
docker run -p 17808:17808 -e STORAGE=sqlite -v kanban:/data ghcr.io/sapn95/golang-kanban:2.0.0

# PostgreSQL
docker run -p 17808:17808 -e DB_HOST=your-postgres -e DB_USER=... -e DB_PASS=... ghcr.io/sapn95/golang-kanban:2.0.0
```

### Prerequisites
With `STORAGE=sqlite` there are none: point `SQLITE_PATH` at a file in a writable directory (`/data` in the image) and the tables are created on first start. One process on one machine, and writes are serialised, which is plenty for a small team; see [docs/adr/0004](docs/adr/0004-sqlite-backend.md) for what that rules out.

PostgreSQL is the external dependency if you pick it. Create a database and a user that owns it; the tables are created by the app on first start (or with `kanban migrate`).

``` sql
CREATE USER kanban WITH PASSWORD 'your_password_here';
CREATE DATABASE kanban OWNER kanban;
```

Upgrading from a version that used the single `cards` table? Take a `pg_dump` first. The old table is imported into a default board on the first start and then dropped.

### Environment Variables
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

# Who has a picture instead of initials, as address=github-login pairs. Unset
# means initials and no outbound request. A GitHub noreply address carries the
# login after the plus sign.
AVATARS=me@example.com=octocat,1+other@users.noreply.github.com=other
```

`kanban` with no arguments serves; `kanban migrate` applies migrations and exits; `kanban version` prints the version. `/healthz` says the process is up, `/readyz` says the database answers, and `/version` says which build is answering — which is how you find out whether a deploy actually landed, without fetching a page and looking for markup only the new version renders.

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

### Building from source
``` bash
go build ./cmd/kanban
go test ./...                       # memory and sqlite backends, no database needed
KANBAN_TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/kanban_test?sslmode=disable go test ./...
```

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
- [ ] ...

#### Contributing
The code layout and the rules it follows are in [docs/architecture.md](docs/architecture.md); every route with what it takes and what it answers is in [docs/api.md](docs/api.md); decisions are recorded in [docs/adr/](docs/adr/).

Contributions are welcome! If you have ideas, bug fixes, or enhancements, feel free to fork the repository, open an issue, or submit a pull request.

#### License
This project is open-sourced under the MIT License.
