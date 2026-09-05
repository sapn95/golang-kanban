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
{{- if not (has .Values.storage (list "postgres" "memory")) -}}
{{- fail (printf "storage must be postgres or memory, got %q" .Values.storage) -}}
{{- end -}}
{{- if and (eq .Values.storage "memory") .Values.postgres.enabled -}}
{{- fail "storage=memory does not use a database: set postgres.enabled=false" -}}
{{- end -}}
{{- if and (eq .Values.storage "postgres") (not .Values.postgres.enabled) (not .Values.externalDatabase.url) (not .Values.externalDatabase.existingSecret) -}}
{{- fail "storage=postgres with postgres.enabled=false needs externalDatabase.url or externalDatabase.existingSecret" -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (eq .Values.storage "memory") -}}
{{- fail "storage=memory keeps state in the pod: replicaCount must be 1, or use postgres" -}}
{{- end -}}
{{- include "kanban.validateDSNParts" . -}}
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
