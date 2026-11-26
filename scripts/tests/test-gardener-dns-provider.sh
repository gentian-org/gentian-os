#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-gardener-dns-provider.sh
# =============================================================================
# dnsProvider gardener (#1366): the zone is written by a Gardener shoot's DNS
# service, from annotations on the edge's Service. What decides it is read
# from the entry in kernel/platforms.yaml, never from the provider's name:
#
#   - the edge chart puts the entry's edgeAnnotations on the Service, rendered
#     against the kernel domain;
#   - an entry without a solver gets no DNS-01 ClusterIssuer and no wildcard;
#   - the interview offers it under HTTP-01, as the default where the cluster
#     serves its detectCRD, and refuses it under DNS-01;
#   - the edge's certificates come from the shoot's certificate service
#     (#1376): the kernel wildcard is a cert.gardener.cloud Certificate, and
#     the operator is told so.
# =============================================================================
# shellcheck disable=SC2016,SC2034 # the checks are strings the harness evaluates, and read rc there

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

SCRIPT_DIR="$(pwd)"
export SCRIPT_DIR
# shellcheck disable=SC2317,SC2329 # called by the functions under test
warn() { printf 'warn: %s\n' "$*" >&2; }
# shellcheck disable=SC2317,SC2329 # called by the functions under test
error() { printf 'error: %s\n' "$*" >&2; }
# shellcheck source=scripts/lib/namespaces.sh
source scripts/lib/namespaces.sh
for fn in yq_get _dns_provider_names _detect_dns_provider _dns_provider_list check_dns_params _dns_param; do
    eval "$(sed -n "/^${fn}() {/,/^}/p" scripts/lib/common.sh)"
done
for fn in gentian_dns_provider gentian_dns_provider_solves gentian_platforms_values gentian_set_args_from_pairs \
    gentian_dns_provider_certificates gentian_kernel_wildcard_issued_by_gardener gentian_cert_manager_namespace \
    kernel_wildcard_current; do
    eval "$(sed -n "/^${fn}() {/,/^}/p" scripts/lib/certs.sh)"
done

# edge_annotations <dnsProvider> <kernelDomain> — the edge Service's annotations, as JSON.
edge_annotations() {
    helm template gentian-edge kernel/manifests/gateway/chart \
        -f kernel/platforms.yaml \
        --set namespace=kernel-edge \
        --set-string platform=openstack \
        --set-string nodeIp=192.0.2.1 \
        --set-string "dnsProvider=$1" \
        --set-string "kernelDomain=$2" 2>&1 |
        python3 -c '
import json, sys, yaml
text = sys.stdin.read()
try:
    docs = list(yaml.safe_load_all(text))
except yaml.YAMLError:
    print(text); sys.exit()
for doc in docs:
    if doc and doc.get("kind") == "EnvoyProxy" and doc["metadata"]["name"] == "gentian-edge":
        print(json.dumps(doc["spec"]["provider"]["kubernetes"]["envoyService"]["annotations"]))
        sys.exit()
print(text)
'
}

# issuers <dnsProvider> — the names of the ClusterIssuers the cert-manager chart renders.
issuers() {
    helm template gentian-cert-manager kernel/manifests/cert-manager/chart \
        -f kernel/platforms.yaml --show-only templates/cluster-issuers.yaml \
        --set-string letsencryptEmail=admin@example.org --set-string kernelDomain=example.org \
        --set-string gatewayNamespace=kernel-edge --set-string gatewayName=perimeter \
        --set-string "dnsProvider=$1" 2>/dev/null |
        python3 -c '
import sys, yaml
print(" ".join(sorted(d["metadata"]["name"] for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "ClusterIssuer")))
'
}

echo ""
echo "dnsProvider gardener"
echo ""

got="$(edge_annotations gardener example.org)"
check "the edge Service names the domain and its wildcard" \
    '[[ "$(jq -r ".\"dns.gardener.cloud/dnsnames\"" <<<"${got}")" == "example.org,*.example.org" ]]' "${got}"
check "the edge Service is in Gardener's class" \
    '[[ "$(jq -r ".\"dns.gardener.cloud/class\"" <<<"${got}")" == garden ]]' "${got}"
check "the platform's annotations stay beside them" \
    '[[ "$(jq -r ".\"loadbalancer.openstack.org/keep-floatingip\"" <<<"${got}")" == true ]]' "${got}"

got="$(edge_annotations cloudflare example.org)"
check "another provider puts nothing of Gardener's on the edge" \
    '[[ "$(jq -r "keys | map(select(startswith(\"dns.gardener.cloud\"))) | length" <<<"${got}")" == 0 ]]' "${got}"

got="$(edge_annotations gardener "")"
check "without a kernel domain the render stops and says why" '[[ "${got}" == *"set kernelDomain"* ]]' "${got}"

got="$(issuers gardener)"
check "no DNS-01 issuer for a provider without a solver" '[[ "${got}" != *dns01* && "${got}" == *http01* ]]' "${got}"
got="$(issuers cloudflare)"
check "a provider with a solver still gets its DNS-01 issuer" '[[ "${got}" == *dns01-cloudflare* ]]' "${got}"

check "gardener does not solve DNS-01" '! DNS_PROVIDER=gardener gentian_dns_provider_solves'
check "none does not solve DNS-01" '! DNS_PROVIDER=none gentian_dns_provider_solves'
check "cloudflare does" 'DNS_PROVIDER=cloudflare gentian_dns_provider_solves'

check "HTTP-01 is offered gardener and none" \
    '[[ " $(_dns_provider_names publishing) " == *" gardener "* && " $(_dns_provider_names publishing) " == *" none "* ]]'
check "DNS-01 is offered neither" \
    '[[ " $(_dns_provider_names solving) " != *" gardener "* && " $(_dns_provider_names solving) " != *" none "* && " $(_dns_provider_names solving) " == *" cloudflare "* ]]'

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT
cat > "${STUB_DIR}/kubectl" <<'STUB'
#!/usr/bin/env bash
# get crd <name> succeeds for the names in KUBECTL_STUB_CRDS.
[[ "$1 $2" == "get crd" && " ${KUBECTL_STUB_CRDS:-} " == *" $3 "* ]]
STUB
chmod +x "${STUB_DIR}/kubectl"
check "a Gardener shoot defaults to gardener" \
    '[[ "$(PATH="${STUB_DIR}:${PATH}" KUBECTL_STUB_CRDS=dnsentries.dns.gardener.cloud _detect_dns_provider)" == gardener ]]'
check "any other cluster defaults to none" \
    '[[ "$(PATH="${STUB_DIR}:${PATH}" KUBECTL_STUB_CRDS= _detect_dns_provider)" == none ]]'

out="$(CERT_ISSUER_MODE=acme-dns01 DNS_PROVIDER=gardener check_dns_params 2>&1)"; rc=$?
check "acme-dns01 with gardener is refused" '(( rc != 0 )) && [[ "${out}" == *"needs a DNS provider that answers DNS-01"* ]]' "${out}"
out="$(CERT_ISSUER_MODE=acme-http01 DNS_PROVIDER=gardener check_dns_params 2>&1)"; rc=$?
check "acme-http01 with gardener passes" '(( rc == 0 ))' "${out}"

check "gardener's certificates are the shoot's" 'DNS_PROVIDER=gardener gentian_kernel_wildcard_issued_by_gardener'
check "cloudflare's are cert-manager's" '! DNS_PROVIDER=cloudflare gentian_kernel_wildcard_issued_by_gardener'
check "none's are cert-manager's" '! DNS_PROVIDER=none gentian_kernel_wildcard_issued_by_gardener'

# kernel_wildcard_current against a stand-in kubectl: Gardener's Certificate
# state and commonName, and the Secret it writes.
cat > "${STUB_DIR}/kubectl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    "get certificates.cert.gardener.cloud wildcard-kernel"*) echo "${STUB_CN} ${STUB_STATE}" ;;
    "get secret wildcard-kernel-tls"*) printf '%s' "${STUB_CRT:-}" ;;
    *) exit 1 ;;
esac
STUB
# shellcheck disable=SC2329 # called by check
current() { PATH="${STUB_DIR}:${PATH}" DNS_PROVIDER=gardener KERNEL_DOMAIN=k.example kernel_wildcard_current; }
check "a Ready shoot certificate with its Secret is current" 'STUB_CN=k.example STUB_STATE=Ready STUB_CRT=Y3J0 current'
check "one still Pending is not" '! STUB_CN=k.example STUB_STATE=Pending STUB_CRT=Y3J0 current'
check "one for another domain is not" '! STUB_CN=old.example STUB_STATE=Ready STUB_CRT=Y3J0 current'
check "one whose Secret is not written yet is not" '! STUB_CN=k.example STUB_STATE=Ready STUB_CRT= current'

# operator_value <dnsProvider> — edgeCertificates as the bootstrap chart hands it to the operator.
operator_value() {
    local t; t="$(mktemp -d)"
    python3 -c 'import yaml; print(yaml.safe_dump({"namespaces": yaml.safe_load(open("kernel/namespaces.yaml"))}))' > "${t}/ns.yaml"
    helm template x kernel/bootstrap/chart -f kernel/platforms.yaml -f "${t}/ns.yaml" -f versions.yaml \
        --set operator.enabled=true --set-string operator.image=x --set-string operator.tag=develop-abc1234 \
        --set-string "dnsProvider=$1" --set-string kernelDomain=k.example --set-string cluster=c \
        -s templates/gentian-os.yaml 2>&1 | sed -n 's/^ *edgeCertificates: //p'
    rm -rf "${t}"
}
check "the operator is told the shoot issues on gardener" '[[ "$(operator_value gardener)" == "\"gardener\"" ]]' "$(operator_value gardener)"
check "and cert-manager elsewhere" '[[ "$(operator_value cloudflare)" == "\"\"" ]]' "$(operator_value cloudflare)"

# token_ref <dnsProvider> <networkMode> — the Cloudflare token Secret the bootstrap chart hands the
# operator: "cleared" when it sets the name empty, "default" when it leaves the chart's own.
token_ref() {
    local t out; t="$(mktemp -d)"
    python3 -c 'import yaml; print(yaml.safe_dump({"namespaces": yaml.safe_load(open("kernel/namespaces.yaml"))}))' > "${t}/ns.yaml"
    out="$(helm template x kernel/bootstrap/chart -f kernel/platforms.yaml -f "${t}/ns.yaml" -f versions.yaml \
        --set operator.enabled=true --set-string operator.image=x --set-string operator.tag=develop-abc1234 \
        --set-string "dnsProvider=$1" --set-string "networkMode=$2" --set-string kernelDomain=k.example --set-string cluster=c \
        -s templates/gentian-os.yaml 2>&1)"
    rm -rf "${t}"
    if grep -A2 '^ *apiTokenSecretRef:' <<<"${out}" | grep -q 'name: ""'; then echo cleared; else echo default; fi
}
check "no Cloudflare token on gardener with a fixed IP" '[[ "$(token_ref gardener static-ip)" == cleared ]]' "$(token_ref gardener static-ip)"
check "nor on none with a fixed IP" '[[ "$(token_ref none static-ip)" == cleared ]]' "$(token_ref none static-ip)"
check "a Cloudflare zone keeps it" '[[ "$(token_ref cloudflare static-ip)" == default ]]' "$(token_ref cloudflare static-ip)"
check "a tunnel keeps it" '[[ "$(token_ref gardener tunnel)" == default ]]' "$(token_ref gardener tunnel)"

# A tenant's certificate is the operator's to ask for and Crossplane's to
# apply: the kind the operator asks for on gardener is one provider-kubernetes
# may manage, in the list and in the role generated from it.
# shellcheck disable=SC2329 # called from the checks' condition strings
provider_may() {
    python3 - "$1" <<'PY'
import sys, yaml
group = sys.argv[1]
kinds = yaml.safe_load(open("crossplane/providers/provider-kubernetes-kinds.yaml"))
listed = any(k["group"] == group and "certificates" in k["resources"] for k in kinds["kinds"])
role = any(
    group in r.get("apiGroups", []) and "certificates" in r.get("resources", [])
    for d in yaml.safe_load_all(open("crossplane/providers/provider-rbac.yaml"))
    if d and d.get("kind") == "ClusterRole"
    for r in d.get("rules", [])
)
sys.exit(0 if listed and role else 1)
PY
}
check "the operator asks for a certificate of the shoot's group" 'grep -q "cert.gardener.cloud" internal/controller/edge_certificates.go'
check "Crossplane may manage the shoot's certificates" 'provider_may cert.gardener.cloud'
check "and cert-manager's" 'provider_may cert-manager.io'

finish
