#!/usr/bin/env bash
# =============================================================================
# scripts/lib/certs.sh — TLS issuers and wildcard certificates.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# cert-manager's DNS-01 propagation check
#
# Before it asks Let's Encrypt to validate, cert-manager checks the challenge
# TXT record itself: it finds the zone by SOA, the zone's nameservers by NS, and
# asks those. By default it resolves both through the cluster's DNS — which here
# cannot answer for the zone. The CoreDNS hairpin serves the kernel domain from
# a hosts block, and a hosts entry answers EVERY query type for its name, so the
# apex's SOA and NS come back NOERROR and empty. cert-manager finds no
# nameservers, and the challenge reports "not yet propagated" while the record
# sits in the zone — for a new tenant's wildcard and for every renewal alike.
#
# So the release is told to resolve through public resolvers instead. Which ones
# is certificates.dns01RecursiveNameservers on the claim; "cluster" leaves
# cert-manager on the cluster's DNS, for a zone only an internal server knows.
# =============================================================================

# cert_manager_dns01_args — the controller flags the claim asks for, one per
# line; nothing for "cluster".
cert_manager_dns01_args() {
    local ns="${DNS01_RECURSIVE_NAMESERVERS:-$(xrd_default certificates.dns01RecursiveNameservers)}"
    if [[ -z "${ns}" || "${ns}" == "cluster" ]]; then
        return 0
    fi
    printf '%s\n' "--dns01-recursive-nameservers-only" \
        "--dns01-recursive-nameservers=${ns}"
}

# _cert_manager_extra_args_json <current-json-array> — the release's extraArgs
# with this installer's DNS-01 flags replaced by the ones the claim asks for.
# Every other flag is kept: an operator who added one meant it.
_cert_manager_extra_args_json() {
    local current="${1:-[]}"
    cert_manager_dns01_args | jq -R . | jq -cs --argjson current "${current}" \
        '[$current[] | select(startswith("--dns01-recursive-nameservers") | not)] + .'
}

# _cert_manager_release_extra_args — extraArgs as the release holds them.
_cert_manager_release_extra_args() {
    helm get values cert-manager -n cert-manager -o json 2>/dev/null |
        jq -c '.extraArgs // []' 2>/dev/null || echo '[]'
}

# cert_manager_dns01_converged — whether the Helm-managed release already runs
# with exactly the DNS-01 flags the claim asks for. Read from the release, not
# the Deployment, because the release is what an upgrade would change.
cert_manager_dns01_converged() {
    helm status cert-manager -n cert-manager >/dev/null 2>&1 || return 0
    local current desired
    current="$(_cert_manager_release_extra_args)"
    desired="$(_cert_manager_extra_args_json "${current}")"
    [[ "$(jq -cS . <<< "${current}")" == "$(jq -cS . <<< "${desired}")" ]]
}

# The zone's host. Cloudflare stays the default so a cluster that never named
# one installs exactly as it did before; every other value is an entry in
# kernel/platforms.yaml.
gentian_dns_provider() { echo "${DNS_PROVIDER:-cloudflare}"; }

# ACME_ENV: production (default) or staging (Let's Encrypt staging API).
# Staging avoids production rate limits; certs are not browser-trusted.
#
# The provider is part of the name because the issuers are per-provider: a
# cluster that switches from Cloudflare to Route 53 gets a new issuer rather
# than one whose solver changed underneath the Certificates pointing at it.
gentian_dns01_cluster_issuer_name() {
    local provider; provider="$(gentian_dns_provider)"
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        echo "letsencrypt-staging-dns01-${provider}"
    else
        echo "letsencrypt-dns01-${provider}"
    fi
}

# The DNS provider's Secret name and OpenBao path, read from the same table the
# charts render from. yq rather than a shell copy of the mapping: a second copy
# is how the issuer and the credential come to disagree about a Secret name.
gentian_dns_credential_secret_name() {
    yq_get ".dnsProviders.$(gentian_dns_provider).credential.secretName" \
        "$(gentian_platforms_values)" 2>/dev/null || true
}

# gentian_cert_manager_namespace — where cert-manager runs on THIS cluster.
#
# Not a constant: the v4 layout gave cert-manager a namespace of its own, the
# v5 layout runs it at the edge beside the Gateway, and a distro addon puts it
# wherever it likes. The webhook Deployment is the one object every
# installation has exactly one of, so it is what the question is asked of, and
# the answer is cached in CERT_MANAGER_NAMESPACE for the rest of the run.
gentian_cert_manager_namespace() {
    # CERT_MANAGER_NAMESPACE carries a default from load time, so an unset
    # variable is not what "unknown" looks like here — a value that no
    # cert-manager answers to is. Check it before trusting it.
    if [[ -n "${CERT_MANAGER_NAMESPACE:-}" ]] \
        && kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE}" >/dev/null 2>&1; then
        echo "${CERT_MANAGER_NAMESPACE}"
        return 0
    fi
    local detected
    detected="$(kubectl get deploy -A -o json 2>/dev/null \
        | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
        | head -1 || true)"
    if [[ -n "${detected}" ]]; then
        CERT_MANAGER_NAMESPACE="${detected}"
        export CERT_MANAGER_NAMESPACE
    fi
    echo "${CERT_MANAGER_NAMESPACE:-cert-manager}"
}

gentian_dns_credential_vault_path() {
    yq_get ".dnsProviders.$(gentian_dns_provider).credential.vaultPath" \
        "$(gentian_platforms_values)" 2>/dev/null || true
}

# Whether this cluster has been given the credential its DNS provider needs.
#
# OpenBao is the record, because that is where every other consumer reads it
# from — the ESO ClusterSecretStore, the credential manager, external-dns.
# Asking the installer's own environment instead is what tied the wildcard to
# CF_API_TOKEN being exported in the shell that happened to run the install.
gentian_dns_credential_present() {
    local path; path="$(gentian_dns_credential_vault_path)"
    [[ -n "${path}" && "${path}" != "null" ]] || return 1
    # BAO_TOKEN is a by-product of initialising the vault, so a run that skips
    # that step because it is already satisfied arrives here with nothing to
    # read OpenBao with — and this function's answer, on a cluster that has the
    # credential, would be "no credential" and the wildcard would be skipped.
    # Reaching for the token here makes the answer about the cluster again.
    if [[ -z "${BAO_TOKEN:-}" ]]; then
        OPENBAO_NAMESPACE="${OPENBAO_NAMESPACE:-$(ns_kernel secrets)}" \
            resolve_openbao_access >/dev/null 2>&1 || return 1
    fi
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    bao kv get -mount=secret "${path}" >/dev/null 2>&1
}

# gentian_platforms_values — the table both charts are rendered against.
gentian_platforms_values() { echo "${SCRIPT_DIR}/kernel/platforms.yaml"; }

# gentian_set_args_from_pairs <prefix> <k=v,k=v> — helm --set-string arguments
# from the flattened maps the claim reader produces.
#
# One parser, three callers: platformParams, dnsParams and the free-form
# lbAnnotations all arrive in the same shape, and each having its own loop is
# how the third one came to skip the malformed-entry warning.
gentian_set_args_from_pairs() {
    local prefix="$1" pairs="${2:-}" pair
    while IFS= read -r pair; do
        [[ -z "${pair}" ]] && continue
        if [[ "${pair}" != *=* ]]; then
            warn "Ignoring malformed ${prefix} entry: ${pair}"
            continue
        fi
        printf -- '--set-string\n%s.%s=%s\n' "${prefix}" "${pair%%=*}" "${pair#*=}"
    done < <(printf '%s\n' "${pairs}" | tr ',' '\n')
}

gentian_cluster_issuers_manifest() {
    # One template per line. ACME_ENV=both installs the staging issuer beside
    # the production one, so a tenant can be pointed at staging while the
    # kernel keeps certificates browsers trust.
    local acme="${ACME_ENV:-production}"
    if [[ "${acme}" != "staging" ]]; then
        echo "cluster-issuers.yaml"
    fi
    if [[ "${acme}" == "staging" || "${acme}" == "both" ]]; then
        echo "cluster-issuers-staging.yaml"
    fi
}

# Apply (or refresh) kernel ClusterIssuers. Safe to re-run
# (./install.sh --step A-06-cluster-issuers).
apply_gentian_cluster_issuers() {
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: skipping ClusterIssuers."
        return
    fi

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${KERNEL_PUBLIC_GATEWAY_NAMESPACE:=$(gentian_services_namespace)}"
    : "${KERNEL_PUBLIC_GATEWAY_NAME:=perimeter}"
    export LETSENCRYPT_EMAIL KERNEL_DOMAIN KERNEL_PUBLIC_GATEWAY_NAMESPACE KERNEL_PUBLIC_GATEWAY_NAME

    if ! command -v helm &>/dev/null; then
        error "helm not found. Aborting."
        exit 1
    fi

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE:-cert-manager}" &>/dev/null; then
        local detected_ns=""
        detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
        if [[ -n "${detected_ns}" ]]; then
            CERT_MANAGER_NAMESPACE="${detected_ns}"
            export CERT_MANAGER_NAMESPACE
        fi
    fi

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE:-cert-manager}" &>/dev/null; then
        error "cert-manager webhook not found; cannot apply ClusterIssuers."
        exit 1
    fi

    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        info "ACME_ENV=staging: using Let's Encrypt staging (untrusted certs, separate rate limits)."
    fi

    local dns_args=()
    while IFS= read -r arg; do dns_args+=("${arg}"); done \
        < <(gentian_set_args_from_pairs dnsParams "${DNS_PARAMS:-}")

    # helm renders one template per --show-only, and there may be two.
    local only=() template
    while IFS= read -r template; do
        only+=(--show-only "templates/${template}")
    done < <(gentian_cluster_issuers_manifest)

    helm template gentian-cert-manager "${SCRIPT_DIR}/kernel/manifests/cert-manager/chart" \
        -f "$(gentian_platforms_values)" \
        "${only[@]}" \
        --set-string letsencryptEmail="${LETSENCRYPT_EMAIL}" \
        --set-string kernelDomain="${KERNEL_DOMAIN}" \
        --set-string gatewayNamespace="${KERNEL_PUBLIC_GATEWAY_NAMESPACE}" \
        --set-string gatewayName="${KERNEL_PUBLIC_GATEWAY_NAME}" \
        --set-string dnsProvider="$(gentian_dns_provider)" \
        "${dns_args[@]+"${dns_args[@]}"}" \
        | kubectl apply -f -
}

# Force cert-manager to re-evaluate the DNS-01-Cloudflare ClusterIssuer's
# readiness. A plain `kubectl apply` (apply_gentian_cluster_issuers above)
# is a no-op whenever the manifest content hasn't changed — no
# resourceVersion bump, no reconcile, nothing — and cert-manager does not
# automatically re-check a ClusterIssuer just because a Secret it depends
# on (cloudflare-api-token) appears later. Confirmed live: repeatedly
# re-applying an unchanged ClusterIssuer left its Ready condition's
# lastTransitionTime exactly where it was, hours later. An annotation
# update, unlike an unchanged apply, does genuinely change the object and
# does trigger the controller's watch-based reconcile.
#
# Call this only once cloudflare-api-token is known to exist (right after
# install_kernel_wildcard creates it, or any time as a day-2 fix via
# ./install.sh --step A-06-cluster-issuers) — calling it before the Secret exists just
# re-confirms NotReady and wastes the wait.
force_reconcile_dns01_cluster_issuer() {
    local issuer
    issuer="$(gentian_dns01_cluster_issuer_name)"
    info "Forcing ${issuer} to re-reconcile..."
    kubectl annotate clusterissuer "${issuer}" \
        "gentian.io/force-reconcile=$(date +%s)" --overwrite >/dev/null
    local i
    for i in {1..30}; do
        if kubectl get clusterissuer "${issuer}" \
                -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
            success "${issuer} is Ready."
            return 0
        fi
        sleep 2
    done
    warn "${issuer} still not Ready after 60s:"
    warn "  kubectl describe clusterissuer ${issuer}"
    return 1
}

# =============================================================================
# Edge address, per provider
#
# Claiming a specific address for a LoadBalancer Service is not portable: every
# provider spells it differently, and AWS NLBs refuse the portable field
# outright. The presets live in kernel/manifests/gateway/chart, keyed on
# lbProvider, with lbAnnotations as the free-form escape hatch — so a new
# provider is a values entry rather than a code change.
#
# Only the detection is here, because it reads the cluster and a template
# cannot. NETWORK_MODE=tunnel never reaches this: the Service stays ClusterIP.
# =============================================================================

# The platform this cluster runs on, when the operator has not said.
#
# PLATFORM decides which entry of kernel/platforms.yaml is applied, and it is
# absent from most claims — so the common case is unset, no preset is emitted,
# and an OpenStack cluster gets a LoadBalancer with no health monitor. That
# failure is silent and intermittent (see the openstack entry), which is the
# worst combination to leave behind a setting somebody has to know to write.
#
# Nodes carry the answer already: spec.providerID is "<cloud>://...". Detection
# only fills a value the operator did not give, so an explicit PLATFORM always
# wins — including PLATFORM="" to opt out deliberately.
#
# Infomaniak is not detectable: its Public Cloud is OpenStack and its nodes say
# so, which is the right answer for the load balancer either way. A cluster that
# wants the name on its claim writes it there.
_detect_platform() {
    local pid
    pid="$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || true)"
    case "${pid}" in
        openstack://*) echo openstack ;;
        aws://*)       echo aws ;;
        gce://*)       echo gcp ;;
        azure://*)     echo azure ;;
        hcloud://*)    echo hetzner ;;
        *)             echo "" ;;
    esac
}


# apply_edge_envoyproxy — the EnvoyProxy the kernel's GatewayClass points at,
# and the GatewayClass itself.
#
# Two shapes, one per network mode.
#
# static-ip: the data plane is a LoadBalancer. Without this the cloud
# controller allocates an arbitrary public address, so NODE_IP — which is what
# DNS and gentian-cluster-config point at — never matches the address traffic
# actually arrives on. See kernel/manifests/gateway/chart for the full
# rationale. It must run before the operator creates the edge Gateways:
# loadBalancerIP is honoured at Service creation only, never on update.
#
# tunnel: nothing outside the cluster connects to the data plane at all — the
# tunnel daemon runs beside it and dials out. A LoadBalancer there asks for an
# address from a cloud that is not there: on a cluster with no load-balancer
# controller the Service sits Pending forever, and a Gateway with no address is
# never Programmed, so every route it carries stays unserved. ClusterIP is both
# what the tunnel needs and something every cluster can give.
apply_edge_envoyproxy() {
    local ns="${ENVOY_GATEWAY_NAMESPACE}"
    local gw_name="${KERNEL_PUBLIC_GATEWAY_NAME:-perimeter}"
    local gw_class="${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}"
    local svc_type=ClusterIP
    [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]] && svc_type=LoadBalancer

    if [[ "${svc_type}" == "ClusterIP" ]]; then
        info "Edge data plane: ClusterIP (NETWORK_MODE=${NETWORK_MODE:-tunnel}; the tunnel dials out)."
        helm template gentian-edge "${SCRIPT_DIR}/kernel/manifests/gateway/chart" \
            -f "$(gentian_platforms_values)" \
            --set "namespace=${ns}" \
            --set-string "envoy.serviceType=ClusterIP" \
            | kubectl apply -f -
        kubectl apply -f "${SCRIPT_DIR}/kernel/manifests/gateway/gatewayclass.yaml"
        kubectl patch gatewayclass "${gw_class}" --type=merge -p \
            "{\"spec\":{\"parametersRef\":{\"group\":\"gateway.envoyproxy.io\",\"kind\":\"EnvoyProxy\",\"name\":\"gentian-edge\",\"namespace\":\"${ns}\"}}}"
        success "EnvoyProxy gentian-edge applied; GatewayClass ${gw_class} points at it."
        return 0
    fi

    # Detection first, and unconditionally.
    #
    # This used to return early when NODE_IP was empty, which read as "no
    # address to pin, nothing to do" — but the EnvoyProxy it renders carries the
    # whole platform profile, not only an address. Hetzner's load balancer has
    # to be placed and stays Pending without its location annotation, and AWS
    # needs its target-type and scheme whether or not an Elastic IP is attached.
    # So a cluster that let its cloud allocate the address got no preset at all,
    # and the failure surfaced as a Service that never gets one.
    #
    # Detection stays here — it reads the cluster, which a template cannot — and
    # the answer is passed in. The presets themselves are in the chart, where
    # the indentation is the template's problem rather than a shell function's
    # guess about a context it cannot see.
    if [[ -z "${PLATFORM+x}" ]]; then
        PLATFORM="$(_detect_platform)"
        [[ -n "${PLATFORM}" ]] &&
            info "  Detected PLATFORM=${PLATFORM} from node providerID."
    fi

    if [[ -n "${NODE_IP:-}" || -n "${EDGE_ADDRESS_REF:-}" ]]; then
        # loadBalancerIP is create-time only, so pinning an already-provisioned
        # data plane silently does nothing. Say so rather than reporting success
        # — the profile below is still applied, because annotations, unlike the
        # address, are honoured on update.
        if kubectl get svc -n "${ns}" \
            -l "gateway.envoyproxy.io/owning-gateway-name=${gw_name}" \
            -o name 2>/dev/null | grep -q .; then
            warn "Envoy data-plane Service already exists; its address applies at creation only."
            warn "  Its current address stands. To re-pin, delete Gateway ${gw_name}"
            warn "  (and its Service) and re-run install.sh."
        else
            local _addr="${NODE_IP}"
            [[ -n "${_addr}" ]] || _addr="${EDGE_ADDRESS_REF}"
            info "Pinning Envoy data-plane LoadBalancer to ${_addr}..."
        fi
    else
        info "No NODE_IP or addressRef; the platform will allocate an edge address."
    fi

    local extra=()
    while IFS= read -r arg; do extra+=("${arg}"); done < <(
        gentian_set_args_from_pairs extraAnnotations "${LB_ANNOTATIONS:-}"
        gentian_set_args_from_pairs platformParams   "${PLATFORM_PARAMS:-}"
    )

    helm template gentian-edge "${SCRIPT_DIR}/kernel/manifests/gateway/chart" \
        -f "$(gentian_platforms_values)" \
        --set "namespace=${ns}" \
        --set-string "nodeIp=${NODE_IP:-}" \
        --set-string "platform=${PLATFORM:-}" \
        --set-string "addressRef=${EDGE_ADDRESS_REF:-}" \
        --set-string "envoy.serviceType=LoadBalancer" \
        "${extra[@]+"${extra[@]}"}" \
        | kubectl apply -f -

    # Create the GatewayClass here rather than waiting for the operator, so the
    # parametersRef is in place before any Gateway exists. ensureGatewayClass
    # reconciles controllerName only, so it leaves this untouched.
    kubectl apply -f "${SCRIPT_DIR}/kernel/manifests/gateway/gatewayclass.yaml"
    kubectl patch gatewayclass "${gw_class}" --type=merge -p \
        "{\"spec\":{\"parametersRef\":{\"group\":\"gateway.envoyproxy.io\",\"kind\":\"EnvoyProxy\",\"name\":\"gentian-edge\",\"namespace\":\"${ns}\"}}}"

    success "Envoy data plane pinned to ${NODE_IP} (EnvoyProxy gentian-edge)."
    info "  The floating IP must already exist and be UNASSOCIATED for the"
    info "  cloud controller to adopt it."
}

wait_for_gateway_platform() {
    if [[ "${ROUTING_MODE:-gateway}" != "gateway" ]]; then
        return 0
    fi
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset; skipping gateway platform wait."
        return 0
    fi

    banner "Waiting for kernel Gateway API platform (ROUTING_MODE=gateway)"
    local ns
    ns="$(_gentian_os_services_namespace)"
    local deadline=$(( SECONDS + 300 ))

    info "Waiting for GatewayClass gentian-envoy (up to 300s)..."
    while (( SECONDS < deadline )); do
        if kubectl get gatewayclass gentian-envoy >/dev/null 2>&1; then
            break
        fi
        sleep 5
    done
    if ! kubectl get gatewayclass gentian-envoy >/dev/null 2>&1; then
        warn "GatewayClass gentian-envoy not found after 300s."
        warn "  Check operator logs: kubectl logs -n $(ns_kernel control) deploy/gentian-os | grep gateway-platform"
        return 1
    fi
    success "GatewayClass gentian-envoy present."

    info "Waiting for Gateway authenticated in ${ns} (up to 300s)..."
    while (( SECONDS < deadline )); do
        if kubectl get gateway -n "${ns}" authenticated >/dev/null 2>&1; then
            break
        fi
        sleep 5
    done
    if ! kubectl get gateway -n "${ns}" authenticated >/dev/null 2>&1; then
        warn "Gateway authenticated not found after 300s."
        return 1
    fi
    success "Gateway authenticated present."

    info "Waiting for kernel HTTPRoutes (up to 300s)..."
    while (( SECONDS < deadline )); do
        local count
        count=$(kubectl get httproute -n "${ns}" -l 'gentianos.io/gateway-component=kernel-route' --no-headers 2>/dev/null | wc -l)
        if [[ "${count}" -ge 4 ]]; then
            success "Kernel HTTPRoutes reconciled (${count} routes)."
            _reconcile_kernel_https_coredns_hairpin
            print_gateway_tunnel_hints
            return 0
        fi
        sleep 5
    done
    warn "Expected kernel HTTPRoutes not ready after 300s."
    warn "  kubectl get gateway,httproute -n ${ns}"
    return 1
}

print_gateway_tunnel_hints() {
    if [[ "${ROUTING_MODE:-gateway}" != "gateway" ]]; then
        return 0
    fi
    local ns; ns="$(gentian_services_namespace)"
    local envoy_ns="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
    info "Gateway API tunnel wiring (${NETWORK_MODE:-tunnel}):"
    info "  Point Cloudflare Tunnel (or your edge proxy) at the Envoy Gateway data plane Service"
    info "  in namespace ${envoy_ns}, not a legacy Ingress controller."
    info "  Discover the Service after the edge Gateways are Programmed:"
    info "    kubectl get svc -n ${envoy_ns} -l gateway.envoyproxy.io/owning-gatewayclass=gentian-envoy"
    info "  Typical origin: https://<envoy-svc>.${envoy_ns}.svc.cluster.local:443"
    info "  Verify: kubectl get gateway -n ${ns} authenticated -o yaml | grep -A5 conditions"
}

# Point CoreDNS kernel HTTPS hairpin entries at the Envoy Gateway ClusterIP.
# mail.<kernelDomain> is left unchanged (Dovecot). The operator reconciles this
# continuously; install/update runs it once so clusters recover before sync.
_reconcile_kernel_https_coredns_hairpin() {
    [[ "${ROUTING_MODE:-gateway}" == "gateway" ]] || return 0
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0

    local envoy_ns="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
    local mail_domain="mail.${KERNEL_DOMAIN}"
    local edge_ip
    edge_ip=$(kubectl get svc -n "${envoy_ns}" \
        -l "gateway.envoyproxy.io/owning-gatewayclass=gentian-envoy" \
        -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null || true)
    if [[ -z "${edge_ip}" ]]; then
        warn "Envoy kernel Gateway Service not found; skipping CoreDNS hairpin update."
        return 0
    fi

    local corefile patched
    corefile=$(kubectl get configmap coredns -n kube-system \
        -o jsonpath='{.data.Corefile}' 2>/dev/null || true)
    if [[ -z "${corefile}" ]]; then
        warn "CoreDNS ConfigMap not found; skipping kernel hairpin update."
        return 0
    fi
    if ! echo "${corefile}" | grep -q "# BEGIN gentian-hairpin"; then
        warn "CoreDNS Corefile has no gentian-hairpin block; operator will create it on sync."
        return 0
    fi

    # Replace legacy Ingress edge IPs for kernel HTTPS hosts; preserve mail entry.
    patched=$(echo "${corefile}" | python3 -c '
import re, sys
corefile = sys.stdin.read()
mail = sys.argv[1]
edge = sys.argv[2]
begin, end = "# BEGIN gentian-hairpin", "# END gentian-hairpin"
i, j = corefile.find(begin), corefile.find(end)
if i < 0 or j < i:
    sys.stdout.write(corefile)
    sys.exit(0)
block = corefile[i:j + len(end)]
lines = []
for line in block.splitlines():
    stripped = line.strip()
    if stripped in (begin, end) or not stripped:
        lines.append(line)
        continue
    parts = stripped.split()
    if len(parts) >= 2 and parts[1] == mail:
        lines.append(line)
        continue
    if len(parts) >= 2 and re.match(r"^\d+\.\d+\.\d+\.\d+$", parts[0]):
        indent = line[: len(line) - len(line.lstrip())]
        lines.append(f"{indent}{edge} {parts[1]}")
        continue
    lines.append(line)
new_block = "\n".join(lines)
sys.stdout.write(corefile[:i] + new_block + corefile[j + len(end):])
' "${mail_domain}" "${edge_ip}")

    if [[ "${corefile}" == "${patched}" ]]; then
        info "CoreDNS kernel hairpin already points at Envoy (${edge_ip})."
        return 0
    fi

    info "Reconciling CoreDNS kernel hairpin → Envoy ${edge_ip}"
    local patch_json
    patch_json=$(printf '%s' "${patched}" | python3 -c \
        'import sys,json; print(json.dumps({"data":{"Corefile":sys.stdin.read()}}))')
    kubectl patch configmap coredns -n kube-system --type=merge -p "${patch_json}" >/dev/null
    kubectl rollout restart deployment coredns -n kube-system >/dev/null 2>&1 || true
    kubectl rollout status deployment coredns -n kube-system --timeout=60s >/dev/null 2>&1 || true
    success "CoreDNS kernel hairpin updated → ${edge_ip}"
}

# =============================================================================
# 12b. Apply the kernel wildcard Certificate + ExternalSecret backing the
# Cloudflare API token. Runs after seed_secrets so the OpenBao path
# `secret/gentian-os/kernel/dns/cloudflare` is populated. Skipped silently
# when CF_API_TOKEN was not provided.
# =============================================================================
# =============================================================================
# The kernel wildcard, after it is issued: who holds a copy, whether they hold
# the RIGHT one, and putting it there.
#
# Separated from install_kernel_wildcard because issuing and distributing need
# different things and fail for different reasons. Issuing needs a DNS
# credential out of OpenBao; copying a Secret that already exists needs
# nothing. Behind one credential gate, a cluster whose certificate had been
# re-issued but whose copies were stale could not be repaired by the step that
# owns those copies: C-01 reported "No credential for DNS provider cloudflare",
# returned 0, printed a tick, and left the Gateway serving the old chain.
# =============================================================================

# _kernel_wildcard_targets — the namespaces that hold a copy, one per line.
#
# One list, read by both the check and the copy. Two lists is how a check comes
# to pass over the namespace the copy writes to.
#
# platform-kernel is the important one: the kernel Gateway lives there (the
# operator creates it in servicesNamespace, whose chart default is
# "platform-kernel" — see charts/gentian-os/values.yaml) and its HTTPS
# listeners reference the wildcard-tls Secret by name. Without the copy both
# listeners sit at ResolvedRefs=False/InvalidCertificateRef, the Gateway never
# reaches Programmed, no address is assigned, Envoy never creates the
# data-plane LoadBalancer, and the cluster answers nothing at all.
#
# app_ns ("gentian-<env>") is kept because the shell half of the installer
# defaults SERVICES_NAMESPACE there — the two halves disagree about which
# namespace is "services", so copy to both rather than pick a side here.
# Namespaces that do not exist are skipped: they are not a gap, they are a
# shape this cluster does not have.
_kernel_wildcard_targets() {
    # Which namespaces hold a copy is a property of the layout: the Gateway
    # reads one, and so does whatever else terminates TLS for a kernel host.
    # C-03 states it in GENTIAN_WILDCARD_TARGETS. Without it there is nothing
    # to guess at -- the list this used to fall back to belonged to the layout
    # that no longer exists, and copying a wildcard into namespaces a cluster
    # does not have is how a step reports satisfied for nothing.
    local target
    for target in ${GENTIAN_WILDCARD_TARGETS:-}; do
        kubectl get namespace "${target}" >/dev/null 2>&1 || continue
        printf '%s\n' "${target}"
    done
}

# kernel_wildcard_propagated — does every copy carry the certificate
# cert-manager actually holds?
#
# The certificate, not the Secret's name. Existence was what this used to be
# asked, and existence is true of a copy made months ago for a domain the
# cluster has since left: C-01 reported satisfied while every kernel hostname
# was served a certificate for the PREVIOUS kernel domain, and the failure
# surfaced three steps later as OpenBao refusing an OIDC discovery document
# whose TLS it could not verify.
#
# Compares the base64 as the API server returns it — same bytes, same
# certificate — so it costs one GET per namespace and needs no openssl.
kernel_wildcard_propagated() {
    local want ns have
    want="$(kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null || true)"
    # Nothing issued yet is not "propagated"; the caller decides what that means.
    [[ -n "${want}" ]] || return 1
    while IFS= read -r ns; do
        [[ -n "${ns}" ]] || continue
        have="$(kubectl get secret wildcard-tls -n "${ns}" \
            -o jsonpath='{.data.tls\.crt}' 2>/dev/null || true)"
        [[ "${have}" == "${want}" ]] || return 1
    done < <(_kernel_wildcard_targets)
    return 0
}

# propagate_kernel_wildcard — copy cert-manager's wildcard into every target.
#
# Idempotent, and safe to call when there is nothing to copy: a cluster whose
# Certificate has not been issued yet returns without complaint, because the
# caller that is about to issue one will call this again afterwards.
propagate_kernel_wildcard() {
    kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" >/dev/null 2>&1 || return 0
    local ns
    while IFS= read -r ns; do
        [[ -n "${ns}" ]] || continue
        info "Propagating wildcard-tls into namespace ${ns}..."
        kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" -o json \
            | python3 -c "
import sys, json
s = json.load(sys.stdin)
for k in ('resourceVersion','uid','creationTimestamp'):
    s['metadata'].pop(k, None)
s['metadata'].pop('annotations', None)
s['metadata']['namespace'] = sys.argv[1]
s['metadata']['name'] = 'wildcard-tls'
print(json.dumps(s))
" "${ns}" | kubectl apply -f -
        success "wildcard-tls propagated to ${ns}."
    done < <(_kernel_wildcard_targets)
}

install_kernel_wildcard() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        return
    fi
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        return
    fi
    local dns_provider; dns_provider="$(gentian_dns_provider)"
    if [[ "${dns_provider}" == "none" ]]; then
        info "No DNS provider for this cluster; skipping the kernel wildcard Certificate."
        info "  Wildcards need DNS-01. Kernel hostnames are served by per-host"
        info "  certificates from the HTTP-01 issuer instead."
        return
    fi
    if ! gentian_dns_credential_present; then
        info "No credential for DNS provider ${dns_provider}; skipping wildcard ISSUANCE."
        info "  Supply it to the credential manager and re-run: ./install.sh --only C-01"
        # Distribution is not issuance and does not need the credential. A
        # certificate cert-manager has already renewed still has to reach the
        # namespaces that serve it, and this is the step that owns that copy --
        # returning here left the only repair path shut on exactly the cluster
        # that needed it. Costs nothing when there is nothing to copy.
        propagate_kernel_wildcard
        return
    fi

    banner "Installing kernel wildcard Certificate"

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    DNS01_CLUSTER_ISSUER="$(gentian_dns01_cluster_issuer_name)"
    export LETSENCRYPT_EMAIL KERNEL_DOMAIN DNS01_CLUSTER_ISSUER

    # 1) ExternalSecret in cert-manager → materializes cloudflare-api-token
    #    Secret from OpenBao. Requires the ClusterSecretStore "openbao" to
    #    exist; install via Argo's globals app or apply directly here as a
    #    fallback (idempotent).
    if ! kubectl get clustersecretstore openbao &>/dev/null; then
        info "ClusterSecretStore/openbao missing — applying directly."
        kubectl apply -f "${SCRIPT_DIR}/kernel/services/_globals/eso-cluster-secret-store.yaml"
    fi
    local dns_args=() secret_name
    while IFS= read -r arg; do dns_args+=("${arg}"); done \
        < <(gentian_set_args_from_pairs dnsParams "${DNS_PARAMS:-}")
    secret_name="$(gentian_dns_credential_secret_name)"

    helm template gentian-cert-manager "${SCRIPT_DIR}/kernel/manifests/cert-manager/chart" \
        -f "$(gentian_platforms_values)" \
        -s templates/dns-credentials-externalsecret.yaml \
        --set-string certManagerNamespace="$(gentian_cert_manager_namespace)" \
        --set-string kernelDomain="${KERNEL_DOMAIN}" \
        --set-string dnsProvider="${dns_provider}" \
        "${dns_args[@]+"${dns_args[@]}"}" \
        | kubectl apply -f -

    # 2) Wait for the underlying Secret to materialize (ESO refresh).
    info "Waiting for Secret $(gentian_cert_manager_namespace)/${secret_name} (max 120s)..."
    local i
    for i in {1..60}; do
        if kubectl get secret "${secret_name}" -n "$(gentian_cert_manager_namespace)" &>/dev/null; then
            success "${secret_name} materialized after ${i}x2s."
            break
        fi
        sleep 2
    done
    if ! kubectl get secret "${secret_name}" -n "$(gentian_cert_manager_namespace)" &>/dev/null; then
        warn "${secret_name} did not materialize within 120s; check ExternalSecret status:"
        warn "  kubectl describe externalsecret ${secret_name} -n $(gentian_cert_manager_namespace)"
        warn "Continuing — wildcard Certificate will issue once the Secret appears."
    fi

    # install_kernel_cert_resources (Step 2b) applies the DNS-01-Cloudflare
    # ClusterIssuer long before this Secret exists (it's only created just
    # above), which leaves it permanently stuck NotReady — see
    # force_reconcile_dns01_cluster_issuer for why and how this fixes it.
    apply_gentian_cluster_issuers
    force_reconcile_dns01_cluster_issuer \
        || warn "Continuing — wildcard Certificate will issue once the issuer recovers."

    # 3) Apply the wildcard Certificate (with domain name templating).
    # The same platform values and DNS arguments every other render of this
    # chart passes, even though this one only wants the wildcard Certificate.
    #
    # -s selects what is PRINTED, not what is evaluated: helm renders every
    # template in the chart and then filters. So dns-credentials-externalsecret
    # .yaml was evaluated here too, its gentian.dns01.profile guard found no
    # dnsProviders table — the chart defaults dnsProvider to cloudflare, which
    # is not "none", so the guard applies — and the render died on
    # "dnsProviders table is empty: render this chart with -f
    # kernel/platforms.yaml". Which is exactly what this call was not doing.
    #
    # That failure was total and silent: `helm template ... | kubectl apply -f -`
    # printed its error, applied nothing, and the step went on to announce the
    # Certificate as applied. So the kernel wildcard Certificate was never
    # created by this path at all.
    #
    # A wildcard kept from an earlier install goes back first, so cert-manager
    # adopts it instead of ordering a new one (see save_kernel_wildcard).
    restore_kernel_wildcard "${DNS01_CLUSTER_ISSUER}"
    helm template gentian-cert-manager "${SCRIPT_DIR}/kernel/manifests/cert-manager/chart" \
        -f "$(gentian_platforms_values)" \
        -s templates/wildcard-kernel-cert.yaml \
        --set-string certManagerNamespace="$(gentian_cert_manager_namespace)" \
        --set-string kernelDomain="${KERNEL_DOMAIN}" \
        --set-string dns01ClusterIssuer="${DNS01_CLUSTER_ISSUER}" \
        --set-string dnsProvider="${dns_provider}" \
        "${dns_args[@]+"${dns_args[@]}"}" \
        | kubectl apply -f -
    success "Kernel wildcard Certificate wildcard-kernel applied in $(gentian_cert_manager_namespace)."
    info "Issuance status:  kubectl get certificate wildcard-kernel -n $(gentian_cert_manager_namespace)"

    # 4) Propagate wildcard-kernel-tls → wildcard-tls in kernel app namespaces.
    #    The Tenant operator issues per-tenant wildcard certs (tenant-*-wildcard-tls),
    #    but the kernel service namespaces are not managed by the operator.
    #    Wait up to 180 s for the cert to be issued first.
    # Current, not merely present. A Secret from the previous issuer exists
    # throughout a reissue -- after a switch from staging to production it
    # stayed for the eight minutes DNS-01 took -- and waiting only for
    # existence copied that old certificate to the Gateway and moved on, so
    # the cluster served staging until somebody re-ran C-03. DNS-01 needs the
    # TXT records to propagate, which takes minutes, not the 180s this was.
    local timeout="${GENTIAN_WILDCARD_WAIT_SECS:-900}"
    local deadline=$(( SECONDS + timeout )) reported=0
    info "Waiting for wildcard-kernel-tls to be issued by ${DNS01_CLUSTER_ISSUER} (max $(( timeout / 60 ))m)..."
    until kernel_wildcard_current; do
        if (( SECONDS > deadline )); then break; fi
        if (( SECONDS - reported >= 60 )); then
            reported=${SECONDS}
            info "  $(kubectl get certificate wildcard-kernel -n "$(gentian_cert_manager_namespace)" \
                -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null)"
        fi
        sleep 5
    done
    local app_ns="gentian-${ENV:-dev}"
    if ! kernel_wildcard_current; then
        warn "wildcard-kernel-tls not yet issued (LE rate-limited or still pending)."
        warn "Re-run install.sh or manually copy the secret once the Certificate is Ready."
        return
    fi
    save_kernel_wildcard
    # Remove stale fallback Certificate CR if present from a prior install.
    if kubectl get certificate wildcard-dev-tls -n "${app_ns}" &>/dev/null; then
        kubectl delete certificate wildcard-dev-tls -n "${app_ns}"
        success "Deleted fallback wildcard-dev-tls Certificate CR from ${app_ns}."
    fi
    propagate_kernel_wildcard

    # ACME staging: trust bundle for in-cluster OIDC clients.
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        local staging_ca_script="${SCRIPT_DIR}/scripts/bootstrap/create-trust-anchor-secret.sh"
        if [[ -x "${staging_ca_script}" ]]; then
            info "Creating gentian-trust-anchor-tls in ${app_ns} (ACME staging)..."
            "${staging_ca_script}" "${app_ns}" || warn "gentian-trust-anchor-tls creation failed (tenant apps may not trust id.${KERNEL_DOMAIN})."
        fi
    fi
}

# =============================================================================
# The kernel wildcard, kept on this host across purges
# =============================================================================
#
# Let's Encrypt allows five certificates per week for the same set of names,
# and a purge throws the wildcard away with its namespace -- so a debugging
# week of reinstalls ran out of production certificates, and dev clusters used
# staging instead, whose chain nothing in the cluster trusts. Keeping the
# issued certificate here and handing it back to cert-manager before it orders
# a new one makes a reinstall cost nothing: cert-manager adopts a Secret whose
# certificate matches the Certificate's names and issuer, and renews it only
# when it is due, which is once in some sixty days however often the cluster
# is rebuilt.
#
# One file per kernel domain, mode 0600, beside the break-glass key that is
# already on this host. It is overwritten rather than accumulated, removed
# the moment it can no longer be used (expired, other names, other ACME
# environment), and removed by --purge --cluster-infra -- the same flag that
# removes the published DNS records, for the same reason.

# kernel_wildcard_current — the wildcard Secret holds what the Certificate
# asks for now: Ready, and issued by the issuer it names. cert-manager records
# the issuer on the Secret, so a certificate left from a previous issuer --
# present, valid, and wrong -- does not pass.
kernel_wildcard_current() {
    local ns want ready have
    ns="$(gentian_cert_manager_namespace)"
    # `|| true` inside: the substitution runs in a subshell that inherits the
    # ERR trap, so a Certificate that is gone -- every purge after the edge
    # namespace went -- printed the installer's abort banner from in there,
    # though the answer is the ordinary "nothing to keep".
    read -r want ready < <(kubectl get certificate wildcard-kernel -n "${ns}" \
        -o jsonpath='{.spec.issuerRef.name} {.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
    [[ -n "${want}" && "${ready}" == "True" ]] || return 1
    have="$(kubectl get secret wildcard-kernel-tls -n "${ns}" \
        -o jsonpath='{.metadata.annotations.cert-manager\.io/issuer-name}' 2>/dev/null)"
    [[ "${have}" == "${want}" ]]
}

gentian_wildcard_cache_file() {
    printf '%s/%s.json' "${GENTIAN_CERT_CACHE:-${HOME}/.gentian/certs}" "${KERNEL_DOMAIN:?}"
}

# save_kernel_wildcard — keep the issued wildcard for the next install.
#
# The Secret as cert-manager wrote it, annotations included: they name the
# issuer and the Certificate, and cert-manager compares them before deciding
# whether a Secret it finds is its own.
save_kernel_wildcard() {
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0
    command -v jq >/dev/null 2>&1 || return 0
    # Only what the claim asks for: mid-reissue the Secret still holds the
    # previous issuer's certificate, and keeping that would keep the wrong one.
    kernel_wildcard_current || return 0
    local file json tmp
    file="$(gentian_wildcard_cache_file)"
    json="$(kubectl get secret wildcard-kernel-tls -n "$(gentian_cert_manager_namespace)" -o json 2>/dev/null \
        | jq -c 'select((.data["tls.crt"] // "") != "" and (.data["tls.key"] // "") != "")
                 | {apiVersion, kind, type, data,
                    metadata: {name: .metadata.name,
                               labels: (.metadata.labels // {}),
                               annotations: ((.metadata.annotations // {})
                                 | with_entries(select(.key | startswith("cert-manager.io/"))))}}' \
        2>/dev/null)" || return 0
    [[ -n "${json}" ]] || return 0
    [[ -f "${file}" && "$(cat "${file}")" == "${json}" ]] && return 0
    mkdir -p "$(dirname "${file}")" && chmod 700 "$(dirname "${file}")" || return 0
    tmp="$(mktemp "${file}.XXXXXX")" || return 0
    chmod 600 "${tmp}"
    if ! { printf '%s\n' "${json}" > "${tmp}" && mv -f "${tmp}" "${file}"; }; then
        rm -f "${tmp}"
        return 0
    fi
    info "Kept the kernel wildcard in ${file} for the next install (rate limits)."
}

# _cached_wildcard_unusable <file> <issuer> — why the copy cannot be reused,
# or nothing when it can.
_cached_wildcard_unusable() {
    local file="$1" issuer="$2" crt key want have ca
    command -v openssl >/dev/null 2>&1 || { echo "openssl is not installed"; return 0; }
    crt="$(jq -r '.data["tls.crt"] // empty' "${file}" 2>/dev/null | base64 -d 2>/dev/null)"
    key="$(jq -r '.data["tls.key"] // empty' "${file}" 2>/dev/null | base64 -d 2>/dev/null)"
    [[ -n "${crt}" && -n "${key}" ]] || { echo "it is not a readable TLS Secret"; return 0; }

    # A day's margin: a certificate that expires mid-install is no saving.
    openssl x509 -noout -checkend 86400 <<< "${crt}" >/dev/null 2>&1 \
        || { echo "it has expired or expires within a day"; return 0; }
    [[ "$(openssl x509 -noout -pubkey <<< "${crt}" 2>/dev/null)" == \
       "$(openssl pkey -pubout <<< "${key}" 2>/dev/null)" ]] \
        || { echo "its key does not match its certificate"; return 0; }

    # Exactly the names the Certificate asks for; anything else and
    # cert-manager would reissue anyway.
    want="$(printf '%s\n' "${KERNEL_DOMAIN}" "*.${KERNEL_DOMAIN}" | sort)"
    have="$(openssl x509 -noout -ext subjectAltName <<< "${crt}" 2>/dev/null \
        | tr ',' '\n' | sed -n 's/^[[:space:]]*DNS://p' | sort)"
    [[ "${have}" == "${want}" ]] || { echo "it covers other names ($(echo "${have}" | tr '\n' ' '))"; return 0; }

    # The ACME environment the claim names now: a staging certificate kept
    # from before a switch to production is exactly the one not to restore.
    ca="$(openssl x509 -noout -issuer <<< "${crt}" 2>/dev/null)"
    if [[ "${issuer}" == *staging* ]]; then
        [[ "${ca}" == *STAGING* ]] || echo "it is a production certificate and the claim asks for staging"
    else
        [[ "${ca}" != *STAGING* ]] || echo "it is a staging certificate and the claim asks for production"
    fi
    return 0
}

# restore_kernel_wildcard <cluster-issuer> — hand a kept wildcard back to
# cert-manager before the Certificate is applied, so it is adopted rather than
# ordered again. Never fatal: without a usable copy, cert-manager issues as it
# always did.
restore_kernel_wildcard() {
    local issuer="$1" ns file why
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0
    command -v jq >/dev/null 2>&1 || return 0
    file="$(gentian_wildcard_cache_file)"
    [[ -f "${file}" ]] || return 0
    ns="$(gentian_cert_manager_namespace)"
    # A live Secret is cert-manager's own and newer than any copy.
    kubectl get secret wildcard-kernel-tls -n "${ns}" >/dev/null 2>&1 && return 0

    why="$(_cached_wildcard_unusable "${file}" "${issuer}")"
    if [[ -n "${why}" ]]; then
        info "Not reusing the kept kernel wildcard: ${why}. Removing ${file}."
        rm -f "${file}"
        return 0
    fi
    # The issuer annotations follow the claim: the copy was checked against
    # its ACME environment above, and a matching name is what lets
    # cert-manager adopt it.
    if jq --arg ns "${ns}" --arg issuer "${issuer}" \
        '.metadata.namespace = $ns
         | .metadata.annotations["cert-manager.io/issuer-name"] = $issuer
         | .metadata.annotations["cert-manager.io/issuer-kind"] = "ClusterIssuer"
         | .metadata.annotations["cert-manager.io/issuer-group"] = "cert-manager.io"
         | .metadata.annotations["cert-manager.io/certificate-name"] = "wildcard-kernel"' \
        "${file}" | kubectl apply -f - >/dev/null; then
        success "Reused the kept kernel wildcard (valid until $(jq -r '.data["tls.crt"]' "${file}" \
            | base64 -d | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)); cert-manager adopts it instead of ordering."
    else
        warn "Could not restore the kept kernel wildcard; cert-manager will issue a new one."
    fi
}

# purge_kernel_wildcard_cache — under --purge --cluster-infra only.
purge_kernel_wildcard_cache() {
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0
    local file; file="$(gentian_wildcard_cache_file)"
    [[ -f "${file}" ]] || return 0
    rm -f "${file}" && success "Removed ${file}."
    rmdir "$(dirname "${file}")" 2>/dev/null || true
}
