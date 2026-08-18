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
