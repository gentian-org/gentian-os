{{- define "liaison.fullname" -}}
{{ default .Release.Name .Values.fullnameOverride }}
{{- end }}

{{- define "liaison.labels" -}}
app.kubernetes.io/name: liaison
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "liaison.selectorLabels" -}}
app.kubernetes.io/name: liaison
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
