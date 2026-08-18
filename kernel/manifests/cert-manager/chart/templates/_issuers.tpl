{{- /*
The kernel ClusterIssuers, once, for both ACME endpoints.

Production and staging differ in exactly two things — the directory URL and a
name prefix — and were previously two full copies. They drifted: the staging
file's header still named a tenant issuer the production one had renamed, and a
change to the DNS-01 solver had to be made twice or the two endpoints would read
different Secrets.

Call with a dict: (dict "root" $ "acmeServer" "…" "prefix" "letsencrypt").
*/ -}}
{{- define "gentian.clusterIssuers" -}}
{{- $root := .root -}}
{{- $prefix := .prefix -}}
{{- $_ := include "gentian.dns01.profile" $root -}}
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: {{ $prefix }}-http01
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/part-of: gentian-os
spec:
  acme:
    server: {{ .acmeServer }}
    email: {{ $root.Values.letsencryptEmail }}
    privateKeySecretRef:
      name: {{ $prefix }}-http01-account-key
    solvers:
      - http01:
          gatewayHTTPRoute:
            parentRefs:
              - group: gateway.networking.k8s.io
                kind: Gateway
                name: {{ $root.Values.gatewayName }}
                namespace: {{ $root.Values.gatewayNamespace }}
{{- $dnsProvider := $root.Values.dnsProvider | default "none" }}
{{- if ne $dnsProvider "none" }}
---
# DNS-01, the only challenge type that can issue a wildcard. The solver block
# below is kernel/platforms.yaml's entry for this provider, passed through
# unchanged — which is why a new provider is a table entry and not a branch.
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: {{ $prefix }}-dns01-{{ $dnsProvider }}
  labels:
    app.kubernetes.io/managed-by: gentian-install
    app.kubernetes.io/part-of: gentian-os
    gentianos.io/dns-provider: {{ $dnsProvider }}
spec:
  acme:
    server: {{ .acmeServer }}
    email: {{ $root.Values.letsencryptEmail }}
    privateKeySecretRef:
      name: {{ $prefix }}-dns01-{{ $dnsProvider }}-account-key
    solvers:
      - dns01:
          {{- include "gentian.dns01.solver" $root | nindent 10 }}
{{- end }}
{{- end -}}
