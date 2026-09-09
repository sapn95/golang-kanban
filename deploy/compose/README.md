# Docker Compose

Three profiles in [`compose.yaml`](compose.yaml), all running the published
image. Pick one:

| Profile    | Storage                        | For                                   |
|------------|--------------------------------|---------------------------------------|
| `sqlite`   | one file on a named volume     | a board on one machine, nothing to install |
| `postgres` | a PostgreSQL container         | a board you expect to grow            |
| `demo`     | memory, written nowhere        | clicking through it once              |

```bash
cd deploy/compose
cp .env.example .env            # set POSTGRES_PASSWORD for the postgres profile
docker compose --profile sqlite up -d
```

The board is on <http://localhost:17808>. `docker compose --profile sqlite down`
stops it and leaves the volume alone.

## When it does not come up

```bash
docker compose --profile sqlite exec kanban-sqlite /kanban doctor
```

That prints every variable the process actually read, with the passwords masked,
and then checks the store, the schema, the identity mode and the backup target.
There is no shell in the image, so `/kanban` is spelled out and `docker compose
exec ... sh` will not work.

## Snapshots

Both persistent profiles write one JSON file with every board to
`/data/snapshots` once a day and keep the last seven. They sit on the same volume
as the database, which covers a bad import or a column deleted by mistake and not
the loss of the volume. To copy them off the host:

```bash
docker compose --profile sqlite cp kanban-sqlite:/data/snapshots ./snapshots
```

To put one back, or to move a board to another machine:

```bash
docker compose --profile sqlite exec -T kanban-sqlite /kanban import -replace < snapshot.json
```

`BACKUP_INTERVAL=0` in `.env` turns the schedule off. A bucket instead of a
directory is `BACKUP_S3_BUCKET` and the variables around it, listed in the
[readme](../../readme.md#environment-variables).

## Upgrading

Change `KANBAN_TAG` in `.env` and bring the profile up again. The schema is
migrated on start.

```bash
docker compose --profile sqlite up -d
```

## Sign-in

Compose deploys the board on its own, so every request is anonymous and whoever
reaches the port can change every board. Two ways out of that: put a proxy that
authenticates in front of it and set `AUTH_MODE=proxy`, or use the
[Helm chart](../helm/kanban), which brings an oauth2-proxy sidecar and the
provider configuration with it.

## A directory instead of a volume

The image runs as uid 65532. A named volume is given the image's ownership when
Docker creates it, so `/data` is writable; a host directory mounted in its place
keeps the host's ownership. If you replace `kanban_data` with a bind mount, make
it writable for that uid first:

```bash
sudo install -d -o 65532 -g 65532 /srv/kanban
```
