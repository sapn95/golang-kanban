# kanban Helm chart

Deploys the board and, depending on `storage`, a Postgres or a volume for it to
use.

```sh
helm install kanban ./deploy/helm/kanban
```

## Storage

`storage: postgres` is the default and expects a database. `postgres.enabled`
decides where that comes from:

| | |
|---|---|
| `postgres.enabled: true` | the chart runs one, with a generated password and a PVC |
| `postgres.enabled: false` | set `externalDatabase.url`, or `externalDatabase.existingSecret` |

`storage: sqlite` runs no database at all. The board is one file on a volume
this chart claims, so there is no second workload to operate, no init container
waiting for it, and the board still survives the pod:

```sh
helm install kanban ./deploy/helm/kanban \
  --set storage=sqlite --set postgres.enabled=false
```

`storage: memory` needs no database and no volume. The board lives in the pod
and is gone when it restarts, which makes it the fastest way to look at the app
and the wrong way to keep anything:

```sh
helm install kanban ./deploy/helm/kanban \
  --set storage=memory --set postgres.enabled=false
```

These combinations fail at template time rather than at runtime: an unknown
backend, `memory` or `sqlite` with a database attached, `postgres` with nowhere
to connect, more than one replica against `memory` or `sqlite`, a `sqlite.path`
that is not absolute or that sits somewhere the chart cannot mount a volume, an
ingress with no hosts or a host with no paths, and a user, database or password
carrying a character that would have to be percent-encoded inside the connection
string.

## Reaching it

`service.type` is `ClusterIP`, so by default you port-forward. For a NodePort:

```sh
helm install kanban ./deploy/helm/kanban \
  --set service.type=NodePort --set service.nodePort=30808
```

Set `ingress.enabled=true` and fill in `ingress.hosts` for anything permanent.

**With `auth.enabled=false` there is no authentication at all.** Anyone who can
reach the service can read and change every board, so keep it on a network you
trust or turn on the sign-in below.

## Sign-in, and TLS with it

`auth.enabled` puts oauth2-proxy in the pod as a sidecar and moves the Service
onto it. From then on nothing reaches the board without a session the proxy
issued, and the app is bound to the pod only. The app itself is unchanged and
knows nothing about any of this — everyone who gets in still shares the same
boards. This decides *who may enter*, not *whose board it is*.

Three providers are pre-wired:

| `auth.provider` | What it needs | What it asks the provider for |
|---|---|---|
| `entra` | `auth.entra.tenant` (a directory id, or `common`), client id and secret | `openid email` |
| `github` | client id and secret, and normally `auth.github.org` or `auth.github.users` | `user:email read:org` |
| `oidc` | `auth.oidc.issuerURL`, client id and secret | `openid email` |

`auth.redirectURL` is your external URL plus `/oauth2/callback`, and it has to
match what you registered with the provider exactly.

### What the consent screen says

The third column is the list the person signing in has to agree to, so the chart
sets it rather than inheriting whatever oauth2-proxy defaults to. The address is
the only thing the sidecar passes to the app, so for `entra` and `oidc` the
address is all it asks for. `auth.scopes` overrides the list: add `profile` to
get the display name and picture the provider holds instead of the name the app
reads out of the address.

GitHub cannot be narrowed, and its consent screen is the reason this section
exists. It offers to let the app "read your organization, team membership, and
private project boards", which is `read:org`, and oauth2-proxy needs that even
with no organisation configured: it calls `/user/orgs` and `/user/teams` on
every sign-in, and GitHub answers 403 to a token without the scope, which fails
the sign-in itself rather than a membership check. The chart refuses a `github`
scope list without `read:org` instead of rendering a sign-in that cannot
complete. Where granting it is not acceptable, `entra` and `oidc` ask for the
address only, and Cloudflare Access in front of the board does the same job
without this sidecar at all.

```sh
helm install kanban ./deploy/helm/kanban \
  --set auth.enabled=true --set auth.provider=github \
  --set auth.github.org=my-org \
  --set auth.clientID=... --set auth.clientSecret=... \
  --set auth.redirectURL=https://board.example.com/oauth2/callback \
  --set auth.tls.enabled=true --set auth.tls.existingSecret=kanban-tls
```

### TLS

oauth2-proxy terminates TLS itself, which is why one component covers both jobs.
Point `auth.tls.existingSecret` at a `kubernetes.io/tls` Secret:

```sh
kubectl create secret tls kanban-tls --cert=cert.pem --key=key.pem
```

TLS **without** sign-in goes through `ingress.tls` instead — the sidecar that
would terminate it only runs when `auth.enabled`. The chart says so rather than
rendering something that cannot work.

One trap the chart refuses rather than lets you find at 2am: `auth.cookie.secure`
is on by default, and a browser silently drops a Secure cookie sent over plain
HTTP, which shows up as a sign-in loop that never completes. Enabling auth with
neither `auth.tls` nor `ingress.tls` therefore fails at template time. Set
`auth.cookie.secure=false` if you really are trialling over HTTP.

The client secret and the generated cookie secret live in a Secret that carries
`helm.sh/resource-policy: keep`, for the same reason the database Secret does:
losing the cookie secret signs out every open session.

## The SQLite volume

`storage: sqlite` creates one `ReadWriteOnce` PersistentVolumeClaim and mounts
it into the app pod. `sqlite.path` is the database file; the mount is the
directory holding it, because SQLite writes the `-wal` and `-shm` files next to
the database and has to create them. A `sqlite.path` sitting directly in `/`, or
anywhere under `/tmp`, is refused: the first would mount the volume over the
container root, the second lands in the `emptyDir` that holds Go's temporary
files and goes away with the pod.

`readOnlyRootFilesystem` stays on. The volume and that `/tmp` are the only
writable paths, and only the volume outlives the pod. `fsGroup: 65532` in
`podSecurityContext` is what lets the non-root user in the image write to it.

One file takes one writer, so the chart refuses `replicaCount` above 1 and the
Deployment switches to `strategy: Recreate`: a rolling update would otherwise
start the second pod while the first still holds the file.

**The claim is kept on uninstall**, for the same reason the Postgres one is: it
is the board. Removing it is a separate, deliberate step:

```sh
kubectl delete pvc -l app.kubernetes.io/instance=<release>
```

There is no `pg_dump` to run against a file. Copying the directory (the database
and its `-wal` sibling) works only while nothing writes to it, which with one
replica means scaling to zero first. The snapshots below need none of that and
are the answer here.

## The bundled Postgres

It is a convenience for a test cluster: one replica, no backups, no failover,
`strategy: Recreate` so two of them never share the volume. Point
`externalDatabase.url` at a real server for anything that matters.

The generated password is read back from the live Secret on later upgrades, so
it is not rotated out from under a running database.

**The PVC and the Secret are both kept on uninstall**, and they have to be: the
volume holds the data and the Secret holds the only copy of the password that
opens it. Keeping one without the other leaves a database nothing can log into.
`helm uninstall` therefore leaves two objects behind, and removing them is a
deliberate, separate step:

```sh
kubectl delete pvc,secret -l app.kubernetes.io/instance=<release>
```

Two limits worth knowing before you rely on the generated password:

- `helm template` has no cluster to read the existing Secret from, so it mints a
  fresh password on every render. **Any GitOps flow that applies rendered output
  must set `postgres.existingSecret`**, naming a Secret that already holds
  `password` and `url`. Without it every sync rewrites the Secret with a new
  password while the database keeps the one it was initialised with, and the
  mismatch stays invisible until the pod restarts and then cannot open its own
  database. `postgres.password` also stops the churn but puts the password in
  whatever holds the values.
- `POSTGRES_PASSWORD` only takes effect when the database initialises. Changing
  `postgres.password` on a release whose volume already exists rolls the app
  onto a credential the database will reject. Change it with `ALTER USER` in the
  database first, or start from an empty volume.

## Backups

The app writes its own snapshots, so there is no sidecar and no CronJob, and the
same file comes out of every backend: one JSON document holding every board,
which `kanban import` reads back into a different deployment or a different
storage backend. The format and what it leaves out is
[`docs/adr/0012`](../../../docs/adr/0012-snapshots-are-the-portable-format.md).

Configuring a target is what starts the schedule. There are two, and the chart
refuses both at once because retention is one number, `backup.keep`:

```sh
# A volume of its own, mounted at backup.dir.path
helm upgrade kanban ./deploy/helm/kanban --set backup.dir.enabled=true

# A bucket, or anything speaking enough of the protocol: MinIO, Garage, Ceph, B2
helm upgrade kanban ./deploy/helm/kanban \
  --set backup.s3.bucket=kanban-snapshots \
  --set backup.s3.region=eu-central-2 \
  --set backup.s3.existingSecret=kanban-backup
```

Snapshots are named `kanban-<timestamp>.json`, the newest `backup.keep` are kept,
and the pruning happens after a successful write. `backup.interval` takes whole
minutes or hours (`24h`, `90m`, `1h30m`), so quote it in a values file: YAML
reads `24h` as a string but `30` as a number, and the app wants the unit. It
refuses seconds, because the app refuses a schedule under a minute and a template
cannot tell `90s` from `90m`. `backup.interval: 0` leaves the target configured
and takes no snapshot until something else asks for one.

### The directory target

`backup.dir.persistence.enabled` claims a volume of its own, which is the default
because a snapshot on the volume it is a copy of survives a bad import and
nothing else. `backup.dir.persistence.existingClaim` puts them on an NFS or
`ReadWriteMany` claim instead, on hardware the database is not on.

`readOnlyRootFilesystem` is on, so a path with no volume under it is a snapshot
that fails at write time, every time, in a log line nobody reads. The chart
refuses that combination: with `backup.dir.persistence.enabled=false` and no
existing claim, the path has to sit inside a volume the pod already has, which
means a directory under `sqlite.path`'s and `storage: sqlite`:

```sh
helm upgrade kanban ./deploy/helm/kanban \
  --set storage=sqlite --set postgres.enabled=false \
  --set backup.dir.enabled=true \
  --set backup.dir.persistence.enabled=false \
  --set backup.dir.path=/data/snapshots
```

The claim the chart creates carries `helm.sh/resource-policy: keep`, like the
other two. A backup volume deleted with the release is gone exactly when the
release it was protecting is.

### The bucket target

The app signs its own requests, in about a hundred lines rather than through the
AWS SDK, and the cost of that is the credential chain: no `~/.aws`, no instance
metadata, no IRSA, no `AssumeRole`. So a bucket needs a key and a secret, and it
needs `backup.s3.region`, which goes into the signature even where the server
ignores it.

`backup.s3.existingSecret` names a Secret holding `access-key-id` and
`secret-access-key`. Without one the chart creates a Secret from
`backup.s3.accessKeyID` and `backup.s3.secretAccessKey`, which is the shorter
path for a `helm install` typed by hand and the wrong one for a GitOps flow that
commits rendered output. That Secret is not kept on uninstall: both values came
from somewhere else, so the copy in the release is not the only one, and a key
left behind is a credential nobody is watching.

`backup.s3.endpoint` points the same code at a self-hosted server, addressed
path-style. `backup.s3.prefix` ends in a slash and lets one bucket hold several
deployments. Temporary credentials work, but nothing in this chart refreshes
one, so `AWS_SESSION_TOKEN` goes in `app.extraEnv` beside whatever does.

With `networkPolicy.enabled` the pod may reach DNS and the bundled Postgres and
nothing else, so a bucket outside the cluster has to be named in
`networkPolicy.allowTo` or every snapshot fails on egress. The chart refuses the
combination rather than punching that hole itself, because how wide it is worth
opening is not the chart's call:

```yaml
networkPolicy:
  enabled: true
  allowTo:
    - to: [{ipBlock: {cidr: 0.0.0.0/0}}]
      ports: [{protocol: TCP, port: 443}]
```

A snapshot is every card, every comment and every assignee's address in plain
text. The directory target writes mode `0600`; a bucket is as private as its
policy.

## Values

See [`values.yaml`](values.yaml); every key is commented there.
