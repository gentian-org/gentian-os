{{- /* The labels every object of the edge carries and its pods are selected by. */ -}}
{{- define "mail-edge.selector" -}}
app.kubernetes.io/name: mail-edge
app.kubernetes.io/instance: mail-edge-{{ .Values.env }}
{{- end -}}

{{- /* The Service of the mail namespace a listener's connections go to. */ -}}
{{- define "mail-edge.backendHost" -}}
{{- $ctx := index . 0 -}}{{- $l := index . 1 -}}
{{- if eq $l.backend "postfix" -}}
postfix-{{ $ctx.Values.env }}-edge.{{ $ctx.Values.mailNamespace }}.svc.cluster.local
{{- else if eq $l.backend "dovecot" -}}
dovecot-{{ $ctx.Values.env }}-edge.{{ $ctx.Values.mailNamespace }}.svc.cluster.local
{{- else -}}
{{- fail (printf "a listener's backend is postfix or dovecot, not %q" $l.backend) -}}
{{- end -}}
{{- end -}}

{{- /* The enabled listeners, by name, in a fixed order. */ -}}
{{- define "mail-edge.enabled" -}}
{{- $out := list -}}
{{- range $name := list "smtp" "submission" "imaps" -}}
{{- if (index $.Values.listeners $name).enabled }}{{ $out = append $out $name }}{{ end -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}
