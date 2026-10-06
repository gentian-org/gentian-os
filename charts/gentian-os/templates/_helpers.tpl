{{/*
Expand the name of the chart.
*/}}
{{- define "gentian-os.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
*/}}
{{- define "gentian-os.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart label value.
*/}}
{{- define "gentian-os.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "gentian-os.labels" -}}
helm.sh/chart: {{ include "gentian-os.chart" . }}
{{ include "gentian-os.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "gentian-os.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gentian-os.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "gentian-os.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "gentian-os.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- /*
The ClusterIssuer per-tenant wildcards are issued by.

Defaults to the cluster's own DNS-01 issuer rather than to Cloudflare's. The
literal "letsencrypt-dns01-cloudflare" was the default in two templates and in
the operator's Go, so a cluster on any other provider had to override it in
three places or issue tenant certificates against an issuer that does not exist
— which surfaces as tenant hostnames served with the wrong certificate, not as
a missing object.

An explicit tenantDNS01ClusterIssuer still wins: a tenant zone is not always the
kernel zone, and a cluster whose tenants live somewhere else needs to say so.
*/ -}}
{{- define "gentian.tenantDNS01ClusterIssuer" -}}
{{- if .Values.tenantDNS01ClusterIssuer -}}
{{- .Values.tenantDNS01ClusterIssuer -}}
{{- else -}}
{{- printf "letsencrypt-dns01-%s" (.Values.dnsProvider | default "cloudflare") -}}
{{- end -}}
{{- end -}}

{{/*
gentian-os.layoutEnv — where each kernel function runs, for the operator to
read (internal/layout). Empty means the v4 layout, which the operator answers
on its own; a v5 install passes the whole map from kernel/namespaces.yaml.
*/}}
{{- define "gentian-os.layoutEnv" -}}
{{- range $fn, $ns := .Values.layout }}
{{- if $ns }}
- name: GENTIAN_NS_{{ $fn | upper | replace "-" "_" }}
  value: {{ $ns | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
One git setting, given through the environment: git reads the numbered pairs
GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n as configuration, up to the number in
GIT_CONFIG_COUNT. Takes .index, .key and .value.
*/}}
{{- define "gentian-os.gitConfigEnv" -}}
- name: GIT_CONFIG_KEY_{{ .index }}
  value: {{ .key }}
- name: GIT_CONFIG_VALUE_{{ .index }}
  value: {{ .value | quote }}
{{- end }}

{{/*
What makes git send a bearer token, for a container that authenticates to the
deployments repository with one: the header, as the setting at .index.

The token is taken from the Secret into a variable of its own first, because
Kubernetes expands $(NAME) only from variables listed earlier in the same
container: the header is put together by the kubelet, and no shell ever
handles the token. With .alone set, this is the container's whole git
environment, and HOME and the count of one are emitted too.
*/}}
{{- define "gentian-os.gitBearerEnv" -}}
{{- if .alone }}
- name: HOME
  value: /tmp
- name: GIT_CONFIG_COUNT
  value: "1"
{{ end -}}
- name: GIT_BEARER_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .secret }}
      key: token
      optional: true
{{ include "gentian-os.gitConfigEnv" (dict "index" .index "key" "http.extraHeader" "value" "Authorization: Bearer $(GIT_BEARER_TOKEN)") }}
{{- end }}
