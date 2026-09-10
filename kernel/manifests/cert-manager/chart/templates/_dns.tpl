{{- /*
The DNS-01 solver, from kernel/platforms.yaml.

Shared by the production and staging issuers, which differ only in their ACME
endpoint — a second copy of this lookup in the staging file is how the two came
to disagree about which Secret the solver reads.

Renders nothing when the provider is "none" or unset, so the caller can decide
whether a missing DNS-01 issuer is an error or the expected shape.
*/ -}}
{{- define "gentian.dns01.profile" -}}
{{- $name := .Values.dnsProvider | default "none" -}}
{{- if ne $name "none" -}}
  {{- if not .Values.dnsProviders -}}
    {{- fail "dnsProviders table is empty: render this chart with -f kernel/platforms.yaml" -}}
  {{- end -}}
  {{- $profile := index .Values.dnsProviders $name | default dict -}}
  {{- if not $profile -}}
    {{- fail (printf "unknown dnsProvider %q — add an entry to kernel/platforms.yaml" $name) -}}
  {{- end -}}
  {{- range $p := (index $profile "requiredParams" | default list) -}}
    {{- if not (index $.Values.dnsParams $p) -}}
      {{- fail (printf "dnsProvider %q requires dnsParams.%s (see kernel/platforms.yaml)" $name $p) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- /*
The solver block itself, rendered through tpl so a provider's zone ids, project
and region come from dnsParams rather than from a template branch per provider.
*/ -}}
{{- define "gentian.dns01.solver" -}}
{{- $profile := index .Values.dnsProviders (.Values.dnsProvider | default "none") | default dict -}}
{{- $solver := index $profile "solver" | default dict -}}
{{- tpl (toYaml $solver) . -}}
{{- end -}}

{{- /* The Secret a provider's solver reads, or "" when it needs none. */ -}}
{{- define "gentian.dns01.secretName" -}}
{{- $profile := index .Values.dnsProviders (.Values.dnsProvider | default "none") | default dict -}}
{{- $cred := index $profile "credential" | default dict -}}
{{- index $cred "secretName" | default "" -}}
{{- end -}}

{{- define "gentian.dns01.vaultPath" -}}
{{- $profile := index .Values.dnsProviders (.Values.dnsProvider | default "none") | default dict -}}
{{- $cred := index $profile "credential" | default dict -}}
{{- index $cred "vaultPath" | default "" -}}
{{- end -}}
