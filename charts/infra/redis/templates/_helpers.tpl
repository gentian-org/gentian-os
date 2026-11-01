{{- define "redis.selectorLabels" -}}
app.kubernetes.io/component: master
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ .Chart.Name }}
{{- end }}

{{- define "redis.labels" -}}
{{ include "redis.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}
