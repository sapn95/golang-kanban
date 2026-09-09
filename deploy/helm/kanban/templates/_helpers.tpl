{{- define "kanban.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kanban.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
54 characters, not 63: "-postgres" needs nine of them. Truncating the joined
string instead would cut the suffix back off and render the Postgres objects
under the app's own name, so the two Deployments would fight over one Service.
*/}}
{{- define "kanban.postgresFullname" -}}
{{- printf "%s-postgres" (include "kanban.fullname" . | trunc 54 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
"-data" needs five characters, for the same reason kanban.postgresFullname
reserves nine.
*/}}
{{- define "kanban.sqliteFullname" -}}
{{- printf "%s-data" (include "kanban.fullname" . | trunc 58 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kanban.sqliteDir" -}}
{{- dir .Values.sqlite.path -}}
{{- end -}}

{{/*
"-backups" needs eight characters, for the same reason kanban.postgresFullname
reserves nine.
*/}}
{{- define "kanban.backupFullname" -}}
{{- printf "%s-backups" (include "kanban.fullname" . | trunc 55 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The Secret the pod reads the bucket credentials from: the caller's when they
brought one, otherwise the one this chart creates.
*/}}
{{- define "kanban.backupSecretName" -}}
{{- ((.Values.backup | default dict).s3 | default dict).existingSecret | default (include "kanban.backupFullname" .) -}}
{{- end -}}

{{/*
Whether the directory target needs a volume mounted for it. False means the
operator has told the chart the path is already inside one, which is the
storage=sqlite case.
*/}}
{{- define "kanban.backupMountsAVolume" -}}
{{- $dir := ((.Values.backup | default dict).dir | default dict) -}}
{{- $p := $dir.persistence | default dict -}}
{{- if and $dir.enabled (or $p.enabled $p.existingClaim) -}}true{{- end -}}
{{- end -}}

{{- define "kanban.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kanban.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kanban.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kanban.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kanban.postgresSelectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-postgres" (include "kanban.name" . | trunc 54 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The Postgres password.

Read back from the live Secret when one exists, so an upgrade does not rotate it
out from under a running database. Generated only when there is nothing to read.

Two consequences, both documented in README.md rather than papered over:
  - `helm template` has no cluster to look in, so it mints a fresh password on
    every render. A GitOps flow that applies rendered output must set
    postgres.password explicitly.
  - The Secret carries helm.sh/resource-policy: keep, exactly like the PVC. If
    it did not, an uninstall would destroy the only copy of the password while
    keeping the data it unlocks, and the next install would generate a password
    the retained database has never heard of.
*/}}
{{/*
The Secret both pods read the database credentials from: the caller's when they
brought one, otherwise the one this chart creates.
*/}}
{{- define "kanban.postgresSecretName" -}}
{{- .Values.postgres.existingSecret | default (include "kanban.postgresFullname" .) -}}
{{- end -}}

{{- define "kanban.postgresPassword" -}}
{{- if .Values.postgres.password -}}
{{- .Values.postgres.password -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kanban.postgresFullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "password") -}}
{{- index $existing.data "password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Fail early on a values combination that would deploy something broken.
*/}}
{{- define "kanban.validate" -}}
{{- if not (has .Values.storage (list "postgres" "sqlite" "memory")) -}}
{{- fail (printf "storage must be postgres, sqlite or memory, got %q" .Values.storage) -}}
{{- end -}}
{{- if and (has .Values.storage (list "sqlite" "memory")) .Values.postgres.enabled -}}
{{- fail (printf "storage=%s does not use a database: set postgres.enabled=false" .Values.storage) -}}
{{- end -}}
{{- if and (eq .Values.storage "postgres") (not .Values.postgres.enabled) (not .Values.externalDatabase.url) (not .Values.externalDatabase.existingSecret) -}}
{{- fail "storage=postgres with postgres.enabled=false needs externalDatabase.url or externalDatabase.existingSecret" -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (eq .Values.storage "memory") -}}
{{- fail "storage=memory keeps state in the pod: replicaCount must be 1, or use postgres" -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (eq .Values.storage "sqlite") -}}
{{- fail "storage=sqlite is one file with one writer: replicaCount must be 1, or use postgres" -}}
{{- end -}}
{{- include "kanban.validateSQLite" . -}}
{{- include "kanban.validateDSNParts" . -}}
{{- include "kanban.validateAuth" . -}}
{{- include "kanban.validateBackup" . -}}
{{- end -}}

{{/*
The backup schedule. Everything here is refused at template time because the
alternative is a pod that serves the board perfectly and quietly takes no
backups, which is the failure nobody notices until they need one.
*/}}
{{- define "kanban.validateBackup" -}}
{{- $backup := .Values.backup | default dict -}}
{{- $dir := $backup.dir | default dict -}}
{{- $s3 := $backup.s3 | default dict -}}
{{- $p := $dir.persistence | default dict -}}
{{- if and $dir.enabled $s3.bucket -}}
{{- fail "backup.dir.enabled and backup.s3.bucket: set one, not both. Two targets would need two retention policies and there is one backup.keep." -}}
{{- end -}}
{{- if or $dir.enabled $s3.bucket -}}
{{- /* toString, and not `default ""`: `interval: 0` is an int, and `default`
       reads any zero as absent. */ -}}
{{- $iv := "" -}}
{{- if not (kindIs "invalid" $backup.interval) -}}
{{- $iv = toString $backup.interval -}}
{{- end -}}
{{- /* Minutes and hours only, and whole ones. The app refuses a schedule under a
       minute, so `30s` here would render a pod that exits at startup, and the
       chart has no arithmetic to tell 90s from 90m. That rules out a few
       intervals the app would take, such as `60s`, and the message says so. */ -}}
{{- if not (regexMatch "^(0|([1-9][0-9]*[mh])+)$" $iv) -}}
{{- fail (printf "backup.interval must be whole minutes or hours, such as 24h, 90m or 1h30m, got %q. Anything under a minute is refused by the app, and 0 turns the schedule off. Quote it in a values file: YAML reads 24h as a string but 30 as a number, and the app wants the unit." $iv) -}}
{{- end -}}
{{- if lt (int ($backup.keep | default 0)) 0 -}}
{{- fail (printf "backup.keep must not be negative, got %v: 0 keeps every snapshot" $backup.keep) -}}
{{- end -}}
{{- end -}}
{{- if $dir.enabled -}}
{{- if not (isAbs ($dir.path | default "")) -}}
{{- fail (printf "backup.dir.path must be an absolute path, got %q" ($dir.path | default "")) -}}
{{- end -}}
{{- if or (eq ($dir.path | default "") "/") (eq (clean ($dir.path | default "")) "/tmp") (hasPrefix "/tmp/" (clean ($dir.path | default ""))) -}}
{{- fail (printf "backup.dir.path must not be / or under /tmp, got %q: /tmp is the emptyDir this pod loses on every restart" $dir.path) -}}
{{- end -}}
{{- if include "kanban.backupMountsAVolume" . -}}
{{- if and (gt (int .Values.replicaCount) 1) (not $p.existingClaim) (eq ($p.accessMode | default "ReadWriteOnce") "ReadWriteOnce") -}}
{{- fail "backup.dir with a ReadWriteOnce claim takes one pod: set replicaCount=1, or backup.dir.persistence.accessMode=ReadWriteMany, or use backup.s3" -}}
{{- end -}}
{{- else -}}
{{- /* Nothing is mounted, so the path has to be inside a volume the pod already
       has. readOnlyRootFilesystem is on, so anywhere else is a snapshot that
       fails at write time, every time, in a log line nobody is reading.
       The one such volume is the SQLite one, and `and` evaluates both sides, so
       the directory is worked out without going through kanban.sqliteDir. */ -}}
{{- $sqliteDir := "" -}}
{{- if eq .Values.storage "sqlite" -}}
{{- with (.Values.sqlite | default dict).path -}}{{- $sqliteDir = dir . -}}{{- end -}}
{{- end -}}
{{- if not (and $sqliteDir (hasPrefix (printf "%s/" $sqliteDir) (printf "%s/" (clean $dir.path)))) -}}
{{- fail (printf "backup.dir.path %q has no volume: enable backup.dir.persistence, or name backup.dir.persistence.existingClaim.%s The root filesystem is read-only, so a path in neither is a snapshot that fails at write time." $dir.path (ternary (printf " Or put it under %s, which storage=sqlite already mounts." $sqliteDir) "" (ne $sqliteDir ""))) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $s3.bucket -}}
{{- if not $s3.region -}}
{{- fail "backup.s3.region is required with backup.s3.bucket: it goes into the request signature, and the S3-compatible servers that ignore it still want one" -}}
{{- end -}}
{{- if and (not $s3.existingSecret) (or (not $s3.accessKeyID) (not $s3.secretAccessKey)) -}}
{{- fail "backup.s3.bucket needs backup.s3.accessKeyID and backup.s3.secretAccessKey, or backup.s3.existingSecret holding access-key-id and secret-access-key. The app signs its own requests and has no credential chain to fall back on." -}}
{{- end -}}
{{- if not (regexMatch "^([A-Za-z0-9._-]+/)*$" ($s3.prefix | default "")) -}}
{{- fail (printf "backup.s3.prefix %q: use letters, digits, dots, dashes, underscores and slashes, do not start with a slash, and end with one" $s3.prefix) -}}
{{- end -}}
{{- if and $s3.endpoint (not (or (hasPrefix "http://" $s3.endpoint) (hasPrefix "https://" $s3.endpoint))) -}}
{{- fail (printf "backup.s3.endpoint %q must be an http or https URL, host name alone is not enough" $s3.endpoint) -}}
{{- end -}}
{{- /* The policy allows DNS and the bundled Postgres. A bucket is neither, and
       the chart cannot know its addresses, so the peer has to be named in
       values rather than guessed here or quietly left out. */ -}}
{{- if and (.Values.networkPolicy | default dict).enabled (not (.Values.networkPolicy | default dict).allowTo) -}}
{{- fail "backup.s3.bucket with networkPolicy.enabled needs the bucket in networkPolicy.allowTo, or every snapshot fails on egress. For a bucket outside the cluster that is:\n  allowTo:\n    - to: [{ipBlock: {cidr: 0.0.0.0/0}}]\n      ports: [{protocol: TCP, port: 443}]" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The database file is not the mount point: SQLite writes <name>-wal and
<name>-shm beside it, so the volume has to be the directory. That makes the
directory the value everything else derives from, and rules out the two paths
that cannot be one: "/" would mount the volume over the container root, and
/tmp is already the emptyDir this pod mounts for Go's temporary files.
*/}}
{{- define "kanban.validateSQLite" -}}
{{- if eq .Values.storage "sqlite" -}}
{{- $path := (.Values.sqlite | default dict).path | default "" -}}
{{- if not (isAbs $path) -}}
{{- fail (printf "sqlite.path must be an absolute path, got %q" $path) -}}
{{- end -}}
{{- if eq (dir $path) "/" -}}
{{- fail (printf "sqlite.path must live in a directory of its own, got %q: the volume is mounted at the file's directory, and that one is the container root" $path) -}}
{{- end -}}
{{- if eq (dir $path) "/tmp" -}}
{{- fail (printf "sqlite.path must not be under /tmp, got %q: /tmp is an emptyDir the pod loses on every restart" $path) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kanban.authFullname" -}}
{{- printf "%s-auth" (include "kanban.fullname" . | trunc 58 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The port the Service should send traffic to. This is the whole switch: with auth
off it is the app, with auth on it is the proxy, and the app is then reachable
only from inside the pod.
*/}}
{{- define "kanban.servicePortName" -}}
{{- /* default dict, because `helm upgrade --reuse-values` carries the values the
       release was installed with and ignores defaults the chart has gained
       since. On a release older than the auth section .Values.auth is nil, and
       a bare field access aborts the upgrade instead of taking the default. */ -}}
{{- $auth := .Values.auth | default dict -}}
{{- $tls := $auth.tls | default dict -}}
{{- if $auth.enabled -}}{{- if $tls.enabled -}}https{{- else -}}auth{{- end -}}{{- else -}}http{{- end -}}
{{- end -}}

{{/*
The cookie secret, on the same terms as the database password: read the live
Secret when there is one, generate only when there is not. oauth2-proxy demands
16, 24 or 32 bytes, so this generates 32.
*/}}
{{- define "kanban.cookieSecret" -}}
{{- if .Values.auth.cookie.secret -}}
{{- .Values.auth.cookie.secret -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kanban.authFullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "cookie-secret") -}}
{{- index $existing.data "cookie-secret" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The OAuth scopes to ask the provider for, as the one space-separated string
oauth2-proxy's --scope takes.

Written down rather than left to the proxy's own default, because the default is
what the consent screen shows the person signing in: "openid email profile" for
an OIDC provider, and "user:email read:org" for GitHub whether or not an
organisation is configured. The chart forwards nothing but the address, so
that is what it asks for. See the comment on auth.scopes in values.yaml for why
GitHub is the exception.
*/}}
{{- define "kanban.authScopes" -}}
{{- $auth := .Values.auth | default dict -}}
{{- if $auth.scopes -}}
{{- join " " $auth.scopes -}}
{{- else if eq $auth.provider "github" -}}
{{- "user:email read:org" -}}
{{- else -}}
{{- "openid email" -}}
{{- end -}}
{{- end -}}

{{- define "kanban.validateAuth" -}}
{{- $auth := .Values.auth | default dict -}}
{{- $tls := $auth.tls | default dict -}}
{{- if $auth.enabled -}}
{{- if not (has $auth.provider (list "entra" "github" "oidc")) -}}
{{- fail (printf "auth.provider must be entra, github or oidc, got %q" $auth.provider) -}}
{{- end -}}
{{- if not $auth.redirectURL -}}
{{- fail "auth.enabled needs auth.redirectURL: the provider rejects a callback it was not registered with. Set it to your external URL plus /oauth2/callback." -}}
{{- end -}}
{{- if and (not $auth.existingSecret) (or (not $auth.clientID) (not $auth.clientSecret)) -}}
{{- fail "auth.enabled needs auth.clientID and auth.clientSecret, or auth.existingSecret holding client-id and client-secret" -}}
{{- end -}}
{{- if and (eq $auth.provider "oidc") (not ($auth.oidc | default dict).issuerURL) -}}
{{- fail "auth.provider=oidc needs auth.oidc.issuerURL" -}}
{{- end -}}
{{- if and (eq $auth.provider "github") $auth.scopes (not (has "read:org" $auth.scopes)) -}}
{{- fail "auth.provider=github needs read:org in auth.scopes: oauth2-proxy reads /user/orgs and /user/teams on every sign-in, with or without auth.github.org, and GitHub answers 403 to a token without that scope, so the sign-in fails instead of the membership check. Use auth.provider=entra or oidc for a sign-in that only asks for the address." -}}
{{- end -}}
{{- if and ($auth.cookie | default dict).secret (not (has (len ($auth.cookie | default dict).secret) (list 16 24 32))) -}}
{{- fail (printf "auth.cookie.secret must be 16, 24 or 32 bytes, got %d" (len ($auth.cookie | default dict).secret)) -}}
{{- end -}}
{{- if and $tls.enabled (not $tls.existingSecret) -}}
{{- fail "auth.tls.enabled needs auth.tls.existingSecret naming a kubernetes.io/tls Secret" -}}
{{- end -}}
{{- if and ($auth.cookie | default dict).secure (not $tls.enabled) (not .Values.ingress.tls) -}}
{{- fail "auth.cookie.secure sends the session cookie only over HTTPS, and nothing here terminates TLS: enable auth.tls, or ingress.tls, or set auth.cookie.secure=false for a plain-HTTP trial" -}}
{{- end -}}
{{- end -}}
{{- if and $tls.enabled (not $auth.enabled) -}}
{{- fail "auth.tls is terminated by the oauth2-proxy sidecar, which only runs when auth.enabled: use ingress.tls for TLS without sign-in" -}}
{{- end -}}
{{- end -}}

{{/*
The connection string is assembled by hand, and Go's urlquery is the wrong
encoder for the userinfo section of a URL: it renders a space as "+", which a
URL parser reads back as a literal plus. Rather than encode and hope, refuse the
characters that would need encoding and say so. The generated password is
alphanumeric and always passes.
*/}}
{{- define "kanban.validateDSNParts" -}}
{{- if and (eq .Values.storage "postgres") .Values.postgres.enabled -}}
{{- $bad := ":/?#[]@!$&'()*+,;= \"" -}}
{{- range $field, $value := dict "postgres.user" .Values.postgres.user "postgres.database" .Values.postgres.database "postgres.password" (default "" .Values.postgres.password) -}}
{{- range $c := splitList "" $bad -}}
{{- if and $value (contains $c $value) -}}
{{- fail (printf "%s must not contain %q: it goes into the DATABASE_URL and would have to be percent-encoded. Use letters, digits, underscore and hyphen." $field $c) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
