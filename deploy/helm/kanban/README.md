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

**There is no authentication in this release.** Anyone who can reach the
service can read and change every board. Keep it on a network you trust.

## Sign-in, and TLS with it

`auth.enabled` puts oauth2-proxy in the pod as a sidecar and moves the Service
onto it. From then on nothing reaches the board without a session the proxy
issued, and the app is bound to the pod only. The app itself is unchanged and
knows nothing about any of this — everyone who gets in still shares the same
boards. This decides *who may enter*, not *whose board it is*.

Three providers are pre-wired:

| `auth.provider` | What it needs |
|---|---|
| `entra` | `auth.entra.tenant` (a directory id, or `common`), client id and secret |
| `github` | client id and secret, and normally `auth.github.org` or `auth.github.users` |
| `oidc` | `auth.oidc.issuerURL`, client id and secret |

`auth.redirectURL` is your external URL plus `/oauth2/callback`, and it has to
match what you registered with the provider exactly.

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

Nothing in this chart takes a backup, and there is no `pg_dump` to run. A backup
is a copy of the directory (the database file and its `-wal` sibling) taken
while nothing writes to it, which with one replica means scaling to zero first.

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
  fresh password on every render. Set `postgres.password` explicitly for any
  GitOps flow that applies rendered output.
- `POSTGRES_PASSWORD` only takes effect when the database initialises. Changing
  `postgres.password` on a release whose volume already exists rolls the app
  onto a credential the database will reject. Change it with `ALTER USER` in the
  database first, or start from an empty volume.

## Values

See [`values.yaml`](values.yaml); every key is commented there.
