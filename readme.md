## Simple Kanban Board
A no-nonsense, lightweight Kanban board built with Golang and HTMX. I couldn’t find a reasonable self-hosted Kanban board that wasn’t using some JavaScript monstrosity like Node or Next.js, I don't need the next [9.1 CVE](https://github.com/advisories/GHSA-f82v-jwr5-mffw) on my server — so I decided to build my own. This project is my sweet little solution to manage tasks simply while learning and sharing a project with the community.

### Overview
Backend: Go, standard library `net/http`, one static binary with everything embedded.

Frontend: HTMX, Tailwind, SortableJS — vendored, nothing is loaded from a CDN, so it works air-gapped.

Database: PostgreSQL (SQLite is next on the list).

### Features
- Boards with as many columns as you like, each with an optional WIP limit.
- Cards with description, due date, labels and a subtask checklist.
- Drag-and-drop between columns with SortableJS; partial updates with HTMX.
- Dark mode.
- Schema migrations run on start; an existing single-table database is imported automatically.

### How it looks
![Screenshot](docs/img/screenshot-v1.0.0.png "Screenshot")

### Using Docker Compose
A sample docker-compose.yml is provided, just use `docker compose up --build`.
This starts PostgreSQL and the board on http://localhost:17808.

### Using the Pre-built Docker Image
``` bash
docker pull ghcr.io/nicolashaas/golang-kanban:latest
docker run -p 17808:17808 -e DB_HOST=your-postgres -e DB_USER=... -e DB_PASS=... ghcr.io/nicolashaas/golang-kanban:latest
```

### Prerequisites
PostgreSQL is the only external dependency. Create a database and a user that owns it; the tables are created by the app on first start (or with `kanban migrate`).

``` sql
CREATE USER kanban WITH PASSWORD 'your_password_here';
CREATE DATABASE kanban OWNER kanban;
```

Upgrading from a version that used the single `cards` table? Take a `pg_dump` first. The old table is imported into a default board on the first start and then dropped.

### Environment Variables
``` bash
SERVER_PORT=17808          # or LISTEN_ADDR=0.0.0.0:17808
STORAGE=postgres           # postgres | memory (memory: nothing is saved, handy for a demo)
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
```

`kanban` with no arguments serves; `kanban migrate` applies migrations and exits; `kanban version` prints the version. `/healthz` says the process is up, `/readyz` says the database answers.

### Building from source
``` bash
go build ./cmd/kanban
go test ./...                       # memory backend only
KANBAN_TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/kanban_test?sslmode=disable go test ./...
```

#### Todo's
If I feel like it I might work on some of these things:
- [x] darkmode
- [ ] remove/add/edit collums (the data model has them; the UI is next)
- [ ] make it pretty
- [ ] add sqlite option for people too lazy to setup a db
- [ ] tls
- [ ] oidc
- [ ] ...

#### Contributing
The code layout and the rules it follows are in [docs/architecture.md](docs/architecture.md); decisions are recorded in [docs/adr/](docs/adr/).

Contributions are welcome! If you have ideas, bug fixes, or enhancements, feel free to fork the repository, open an issue, or submit a pull request.

#### License
This project is open-sourced under the MIT License.
