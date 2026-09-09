# Unraid

[`kanban.xml`](kanban.xml) is a container template for Unraid's Docker tab. It
is not in Community Applications, so install it by hand, either way round:

**Paste the URL.** Docker → Add Container → in the Template field put

```text
https://raw.githubusercontent.com/sapn95/golang-kanban/homelab/deploy/unraid/kanban.xml
```

**Or copy the file.** Put it on the flash drive as
`/boot/config/plugins/dockerMan/templates-user/my-kanban.xml`. It then shows up
in the template list under `User templates` after a page reload.

Either way the form comes up filled in: port 17808, `/data` on
`/mnt/user/appdata/kanban`, SQLite, and a daily snapshot into
`/data/snapshots`. Apply, and the board is on `http://<tower>:17808`.

## What the template sets that you would otherwise miss

`--user 99:100` in Extra Parameters. The image runs as uid 65532 and
`/mnt/user/appdata` belongs to `nobody:users`, which is 99:100 on Unraid, so
without it the container starts and then cannot write its database.

## No shell in the image

It is a distroless image, so the Console button in the Unraid UI opens a
terminal with nothing to run in it. The binary answers questions itself:

```bash
docker exec kanban /kanban doctor
```

That prints every variable the process read, passwords masked, then checks the
store, the schema, the identity mode and the snapshot target. `docker logs
kanban` is the other place to look.

## Snapshots

`BACKUP_DIR=/data/snapshots` writes one JSON file holding every board once a day
and keeps the last seven. They land in appdata, so an appdata backup covers
them. Reading one back:

```bash
docker exec -i kanban /kanban import -replace < /mnt/user/appdata/kanban/snapshots/kanban-20260909T161209Z.json
```

## PostgreSQL instead

Set `STORAGE=postgres` and fill in `DB_HOST`, `DB_USER`, `DB_PASS` and
`DB_NAME` under Advanced view. Point them at your own PostgreSQL container; the
template does not bring one. The schema is created on start.

## Sign-in

The board itself does not authenticate, so anyone who reaches the port can
change any card. On a LAN that is usually the point. Behind a reverse proxy that
does authenticate, set `AUTH_MODE=proxy` and `AUTH_HEADER` to the header the
proxy sets, and the board shows who did what. For an identity provider without a
proxy in front, the [Helm chart](../helm/kanban) is the deployment that brings
one.
