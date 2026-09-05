# kanban Helm chart

Deploys the board and, optionally, a Postgres for it to use.

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

`storage: memory` needs no database at all. The board lives in the pod and is
gone when it restarts, which makes it the fastest way to look at the app and
the wrong way to keep anything:

```sh
helm install kanban ./deploy/helm/kanban \
  --set storage=memory --set postgres.enabled=false
```

These combinations fail at template time rather than at runtime: an unknown
backend, `memory` with a database attached, `postgres` with nowhere to connect,
more than one replica against `memory`, an ingress with no hosts or a host with
no paths, and a user, database or password carrying a character that would have
to be percent-encoded inside the connection string.

## Reaching it

`service.type` is `ClusterIP`, so by default you port-forward. For a NodePort:

```sh
helm install kanban ./deploy/helm/kanban \
  --set service.type=NodePort --set service.nodePort=30808
```

Set `ingress.enabled=true` and fill in `ingress.hosts` for anything permanent.

**There is no authentication in this release.** Anyone who can reach the
service can read and change every board. Keep it on a network you trust.

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
