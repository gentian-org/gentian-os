{{/*
ns — the kernel namespace with a function, from kernel/namespaces.yaml as the
installer passed it in: {{ include "ns" (list . "edge") }}. A function the
layout does not have fails the render, which is the point: nothing here can
address a namespace by a name of its own.
*/}}
{{- define "ns" -}}
{{- $root := index . 0 -}}
{{- $fn := index . 1 -}}
{{- $found := "" -}}
{{- range $root.Values.namespaces.kernel }}{{ if eq .function $fn }}{{ $found = .name }}{{ end }}{{ end -}}
{{- if not $found }}{{ fail (printf "kernel/namespaces.yaml has no kernel namespace with function %q (was it passed as .Values.namespaces?)" $fn) }}{{ end -}}
{{- $found -}}
{{- end -}}

{{- /*
  gentianOsRevision: the git revision every Application on the gentian-os
  repository follows. The installer passes this checkout's commit, which the
  operator image is built from (gentian_os_revision); the branch only where
  it passes none.
*/ -}}
{{- define "gentianOsRevision" -}}
{{ .Values.gentianOsRevision | default .Values.gentianOsBranch }}
{{- end -}}
