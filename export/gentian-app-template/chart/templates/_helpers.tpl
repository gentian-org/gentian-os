{{- define "gentian-app.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gentian-app.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "gentian-app.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
