#!/usr/bin/env bash
# =============================================================================
# scripts/lib/certs.sh — cert-manager, TLS issuers, Envoy Gateway, wildcard certificates.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# 2. Install cert-manager via Helm
# =============================================================================
install_cert_manager() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping cert-manager installation."
        return
    fi

    banner "Installing cert-manager"

    if helm status cert-manager -n cert-manager &>/dev/null; then
        # Existing Helm release may have been created by a previous install.sh run.
        : "${GENTIAN_MANAGED_CERT_MANAGER:=1}"
        save_install_state
        success "cert-manager already installed (Helm release present). Skipping."
        return
    fi

    # Discover an existing non-Helm cert-manager install by finding the
    # webhook deployment/service in any namespace.
    local detected_ns=""
    detected_ns=$(kubectl get deploy -A -o json 2>/dev/null \
        | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
        | head -1 || true)

    if [[ -z "${detected_ns}" ]]; then
        detected_ns=$(kubectl get svc -A -o json 2>/dev/null \
            | jq -r '.items[] | select(.metadata.name=="cert-manager-webhook") | .metadata.namespace' \
            | head -1 || true)
    fi

    # Only skip Helm if a real cert-manager installation is present.
    if [[ -n "${detected_ns}" ]]; then
        CERT_MANAGER_NAMESPACE="${detected_ns}"
        export CERT_MANAGER_NAMESPACE
        GENTIAN_MANAGED_CERT_MANAGER="0"
        save_install_state
        warn "cert-manager already present but not managed by Helm (e.g. distro addon)."
        info "Detected cert-manager webhook in namespace ${CERT_MANAGER_NAMESPACE}; using that installation as-is."
        return
    fi

    # Stale CRDs can remain after a partial uninstall; do not treat that as a
    # valid installation. Proceed with Helm install if webhook/service is absent.
    local has_existing_crds="0"
    if kubectl get crd certificates.cert-manager.io &>/dev/null; then
        has_existing_crds="1"
        warn "cert-manager CRDs exist, but no cert-manager webhook/service was detected."
        warn "Proceeding with Helm install while reusing existing CRDs."
    fi

    helm repo add jetstack "$(gentian_pin cert-manager repo)" --force-update
    helm repo update
    if [[ "${has_existing_crds}" == "1" ]]; then
        # Existing CRDs may come from distro addons or prior non-Helm installs;
        # do not ask Helm to import/manage them.
        helm upgrade --install cert-manager jetstack/cert-manager \
            -n cert-manager \
            --version "$(gentian_pin cert-manager chart)" \
            --create-namespace \
            --set crds.enabled=false \
            --wait --timeout 5m
    else
        helm upgrade --install cert-manager jetstack/cert-manager \
            -n cert-manager \
            --version "$(gentian_pin cert-manager chart)" \
            --create-namespace \
            --set crds.enabled=true \
            --wait --timeout 5m
    fi
    CERT_MANAGER_NAMESPACE="cert-manager"
    export CERT_MANAGER_NAMESPACE
    GENTIAN_MANAGED_CERT_MANAGER="1"
    save_install_state
    success "cert-manager installed."
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
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        echo "cluster-issuers-staging.yaml"
    else
        echo "cluster-issuers.yaml"
    fi
}

# Apply (or refresh) kernel ClusterIssuers. Safe to re-run (update.sh --acme-issuers).
apply_gentian_cluster_issuers() {
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: skipping ClusterIssuers."
        return
    fi

    : "${LETSENCRYPT_EMAIL:=admin@${KERNEL_DOMAIN}}"
    : "${KERNEL_PUBLIC_GATEWAY_NAMESPACE:=$(gentian_services_namespace)}"
    : "${KERNEL_PUBLIC_GATEWAY_NAME:=kernel-public-gateway}"
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

    helm template gentian-cert-manager "${SCRIPT_DIR}/kernel/manifests/cert-manager/chart" \
        -f "$(gentian_platforms_values)" \
        -s "templates/$(gentian_cluster_issuers_manifest)" \
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
# update.sh --acme-issuers) — calling it before the Secret exists just
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
# 2b. Install kernel cert-manager ClusterIssuers (always — both HTTP-01 and
# DNS-01-Cloudflare). The wildcard Certificate + cloudflare-api-token
# ExternalSecret are applied later by `install_kernel_wildcard` (after the
# OpenBao seeding step has populated the token). See docs/design/multi-tenancy.md §3.
# =============================================================================
install_kernel_cert_resources() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping kernel cert-manager resources."
        return
    fi
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset: skipping kernel cert-manager resources."
        return
    fi

    banner "Installing kernel cert-manager ClusterIssuers"

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
    : "${CERT_MANAGER_NAMESPACE:=cert-manager}"
    export CERT_MANAGER_NAMESPACE

    if ! kubectl get deploy cert-manager-webhook -n "${CERT_MANAGER_NAMESPACE}" &>/dev/null; then
        error "cert-manager webhook deployment not found in namespace ${CERT_MANAGER_NAMESPACE}."
        error "Fix cert-manager first, then re-run install.sh."
        exit 1
    fi

    # Wait for cert-manager webhook to be ready (Certificate/ClusterIssuer
    # admission would otherwise be rejected by an uninitialized webhook).
    info "Waiting for cert-manager webhook to be ready in namespace ${CERT_MANAGER_NAMESPACE}..."
    kubectl rollout status -n "${CERT_MANAGER_NAMESPACE}" deploy/cert-manager-webhook --timeout=180s >/dev/null \
        || warn "cert-manager-webhook not Ready within 180s (continuing)."

    apply_gentian_cluster_issuers
    local http01_name="letsencrypt-http01"
    [[ "${ACME_ENV:-production}" == "staging" ]] && http01_name="letsencrypt-staging-http01"
    if [[ "$(gentian_dns_provider)" == "none" ]]; then
        success "ClusterIssuer ${http01_name} applied."
        info "  No DNS provider: this cluster issues per-host certificates, not wildcards."
    else
        success "ClusterIssuers ${http01_name} and $(gentian_dns01_cluster_issuer_name) applied."
    fi
}

# =============================================================================
# 2c. Install Envoy Gateway (Gateway API edge stack)
# =============================================================================
install_envoy_gateway() {
    if [[ "$INSTALL_CLUSTER_INFRA" != "1" ]]; then
        warn "Cluster infra disabled: skipping Envoy Gateway installation."
        return
    fi

    : "${ROUTING_MODE:=gateway}"
    export ROUTING_MODE
    if [[ "${ROUTING_MODE}" != "gateway" ]]; then
        error "ROUTING_MODE=${ROUTING_MODE} is no longer supported; use ROUTING_MODE=gateway."
        exit 1
    fi

    banner "Installing Envoy Gateway and Gateway API CRDs"

    local ns="${ENVOY_GATEWAY_NAMESPACE}"
    local chart_version="${ENVOY_GATEWAY_CHART_VERSION}"
    local svc_type="ClusterIP"
    if [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]]; then
        svc_type="LoadBalancer"
    fi

    if helm status eg -n "${ns}" &>/dev/null; then
        success "Envoy Gateway Helm release already present in ${ns}."
    else
        info "Installing Envoy Gateway ${chart_version} (service type ${svc_type})..."
        helm upgrade --install eg "$(gentian_pin envoy-gateway repo)" \
            --version "${chart_version}" \
            -n "${ns}" \
            --create-namespace \
            --set "config.envoyGateway.gateway.controllerName=${GENTIAN_GATEWAY_CONTROLLER_NAME}" \
            --set deployment.replicas=1 \
            --set "kubernetesService.type=${svc_type}" \
            --wait --timeout 5m
        success "Envoy Gateway installed in namespace ${ns}."
    fi

    info "Waiting for Envoy Gateway controller deployment..."
    if ! kubectl rollout status -n "${ns}" deploy/envoy-gateway --timeout=180s >/dev/null 2>&1; then
        warn "Envoy Gateway deployment not Ready within 180s (continuing)."
    fi

    info "Verifying Gateway API CRDs..."
    local crd
    for crd in \
        gatewayclasses.gateway.networking.k8s.io \
        gateways.gateway.networking.k8s.io \
        httproutes.gateway.networking.k8s.io; do
        if kubectl get crd "${crd}" &>/dev/null; then
            kubectl wait --for=condition=Established "crd/${crd}" --timeout=120s >/dev/null 2>&1 \
                || warn "CRD ${crd} not Established within 120s."
        else
            warn "Gateway API CRD ${crd} not found after Envoy Gateway install."
        fi
    done
    success "Envoy Gateway and Gateway API CRDs ready (ROUTING_MODE=gateway)."
    info "  GatewayClass: ${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}"
    info "  Controller:   ${GENTIAN_GATEWAY_CONTROLLER_NAME}"
    info "  Status:       kubectl get gatewayclass,gateway -A"

    _pin_static_ip_edge_address
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


# Pin the Envoy data-plane LoadBalancer to NODE_IP (NETWORK_MODE=static-ip only).
#
# Without this the cloud controller allocates an arbitrary public IP for the
# Envoy Service, so NODE_IP — which is what DNS and gentian-cluster-config point
# at — never matches the address traffic actually arrives on. See
# kernel/manifests/gateway/chart for the full rationale.
#
# Must run before the operator creates kernel-public-gateway: loadBalancerIP is
# honoured at Service creation only, never on update.
_pin_static_ip_edge_address() {
    [[ "${NETWORK_MODE:-tunnel}" == "static-ip" ]] || return 0

    local ns="${ENVOY_GATEWAY_NAMESPACE}"
    local gw_name="${KERNEL_PUBLIC_GATEWAY_NAME:-kernel-public-gateway}"
    local gw_class="${GENTIAN_GATEWAY_CLASS_NAME:-gentian-envoy}"

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

# =============================================================================
# apply_kernel_gateway_overlays — gateway-mode Helm value overlays
# =============================================================================
apply_kernel_gateway_overlays() {
    if [[ "${ROUTING_MODE:-gateway}" != "gateway" ]]; then
        error "ROUTING_MODE=${ROUTING_MODE} is no longer supported; use ROUTING_MODE=gateway."
        exit 1
    fi
    info "Applying kernel gateway value overlays (ROUTING_MODE=gateway)..."
    success "Kernel gateway overlays ready."
    print_gateway_tunnel_hints || true
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
        warn "  Check operator logs: kubectl logs -n gentian-system deploy/gentian-os | grep gateway-platform"
        return 1
    fi
    success "GatewayClass gentian-envoy present."

    info "Waiting for Gateway kernel-public-gateway in ${ns} (up to 300s)..."
    while (( SECONDS < deadline )); do
        if kubectl get gateway -n "${ns}" kernel-public-gateway >/dev/null 2>&1; then
            break
        fi
        sleep 5
    done
    if ! kubectl get gateway -n "${ns}" kernel-public-gateway >/dev/null 2>&1; then
        warn "Gateway kernel-public-gateway not found after 300s."
        return 1
    fi
    success "Gateway kernel-public-gateway present."

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
    info "  Discover the Service after kernel-public-gateway is Programmed:"
    info "    kubectl get svc -n ${envoy_ns} -l gateway.envoyproxy.io/owning-gateway-name=kernel-public-gateway"
    info "  Typical origin: https://<envoy-svc>.${envoy_ns}.svc.cluster.local:443"
    info "  Verify: kubectl get gateway -n ${ns} kernel-public-gateway -o yaml | grep -A5 conditions"
}

# Point CoreDNS kernel HTTPS hairpin entries at the Envoy Gateway ClusterIP.
# mail.<kernelDomain> is left unchanged (Dovecot). The operator reconciles this
# continuously; install/update runs it once so clusters recover before sync.
_reconcile_kernel_https_coredns_hairpin() {
    [[ "${ROUTING_MODE:-gateway}" == "gateway" ]] || return 0
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0

    local services_ns; services_ns="$(gentian_services_namespace)"
    local envoy_ns="${ENVOY_GATEWAY_NAMESPACE:-envoy-gateway-system}"
    local mail_domain="mail.${KERNEL_DOMAIN}"
    local edge_ip
    edge_ip=$(kubectl get svc -n "${envoy_ns}" \
        -l "gateway.envoyproxy.io/owning-gateway-name=kernel-public-gateway,gateway.envoyproxy.io/owning-gateway-namespace=${services_ns}" \
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
        info "No credential for DNS provider ${dns_provider}; skipping the kernel wildcard Certificate."
        info "  Supply it to the credential manager and re-run: ./install.sh --only C-01"
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
        --set-string kernelDomain="${KERNEL_DOMAIN}" \
        --set-string dnsProvider="${dns_provider}" \
        "${dns_args[@]+"${dns_args[@]}"}" \
        | kubectl apply -f -

    # 2) Wait for the underlying Secret to materialize (ESO refresh).
    info "Waiting for Secret cert-manager/${secret_name} (max 120s)..."
    local i
    for i in {1..60}; do
        if kubectl get secret "${secret_name}" -n cert-manager &>/dev/null; then
            success "${secret_name} materialized after ${i}x2s."
            break
        fi
        sleep 2
    done
    if ! kubectl get secret "${secret_name}" -n cert-manager &>/dev/null; then
        warn "${secret_name} did not materialize within 120s; check ExternalSecret status:"
        warn "  kubectl describe externalsecret ${secret_name} -n cert-manager"
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
    helm template gentian-cert-manager "${SCRIPT_DIR}/kernel/manifests/cert-manager/chart" \
        -s templates/wildcard-kernel-cert.yaml \
        --set-string kernelDomain="${KERNEL_DOMAIN}" \
        --set-string dns01ClusterIssuer="${DNS01_CLUSTER_ISSUER}" \
        | kubectl apply -f -
    success "Kernel wildcard Certificate wildcard-kernel applied (cert-manager namespace)."
    info "Issuance status:  kubectl get certificate wildcard-kernel -n cert-manager"

    # 4) Propagate wildcard-kernel-tls → wildcard-tls in kernel app namespaces.
    #    The Tenant operator issues per-tenant wildcard certs (tenant-*-wildcard-tls),
    #    but the kernel service namespaces are not managed by the operator.
    #    Wait up to 180 s for the cert to be issued first.
    info "Waiting for wildcard-kernel-tls to be issued (max 180s)..."
    local i
    for i in {1..90}; do
        if kubectl get secret wildcard-kernel-tls -n cert-manager &>/dev/null; then
            success "wildcard-kernel-tls Secret exists after ${i}x2s."
            break
        fi
        sleep 2
    done
    local app_ns="gentian-${ENV:-dev}"
    if ! kubectl get secret wildcard-kernel-tls -n cert-manager &>/dev/null; then
        warn "wildcard-kernel-tls not yet issued (LE rate-limited or still pending)."
        warn "Re-run install.sh or manually copy the secret once the Certificate is Ready."
        return
    fi
    # Remove stale fallback Certificate CR if present from a prior install.
    if kubectl get certificate wildcard-dev-tls -n "${app_ns}" &>/dev/null; then
        kubectl delete certificate wildcard-dev-tls -n "${app_ns}"
        success "Deleted fallback wildcard-dev-tls Certificate CR from ${app_ns}."
    fi
    # Propagate to all namespaces that reference wildcard-tls.
    #
    # platform-kernel is the important one and was missing: the kernel Gateway
    # lives there (the operator creates it in servicesNamespace, whose chart
    # default is "platform-kernel" — see charts/gentian-os/values.yaml), and its
    # HTTPS listeners reference the wildcard-tls Secret by name. Without the copy
    # both listeners sit at ResolvedRefs=False/InvalidCertificateRef, the Gateway
    # never reaches Programmed, no address is assigned, Envoy never creates the
    # data-plane LoadBalancer, and the cluster answers nothing at all.
    #
    # app_ns ("gentian-<env>") is kept because the shell half of the installer
    # defaults SERVICES_NAMESPACE there — the two halves disagree about which
    # namespace is "services", so copy to both rather than pick a side here.
    local _wc_targets=("${app_ns}" "$(gentian_services_namespace)" argocd)
    local _wc_seen=""
    for _wc_ns in "${_wc_targets[@]}"; do
        [[ " ${_wc_seen} " == *" ${_wc_ns} "* ]] && continue
        _wc_seen+=" ${_wc_ns}"
        kubectl get namespace "${_wc_ns}" >/dev/null 2>&1 || continue
        info "Propagating wildcard-tls into namespace ${_wc_ns}..."
        kubectl get secret wildcard-kernel-tls -n cert-manager -o json \
            | python3 -c "
import sys, json
s = json.load(sys.stdin)
for k in ('resourceVersion','uid','creationTimestamp'):
    s['metadata'].pop(k, None)
s['metadata'].pop('annotations', None)
s['metadata']['namespace'] = sys.argv[1]
s['metadata']['name'] = 'wildcard-tls'
print(json.dumps(s))
" "${_wc_ns}" | kubectl apply -f -
        success "wildcard-tls propagated to ${_wc_ns}."
    done

    # ACME staging: trust bundle for in-cluster OIDC clients.
    if [[ "${ACME_ENV:-production}" == "staging" ]]; then
        local staging_ca_script="${SCRIPT_DIR}/scripts/bootstrap/create-staging-ca-secret.sh"
        if [[ -x "${staging_ca_script}" ]]; then
            info "Creating gentian-staging-ca-tls in ${app_ns} (ACME staging)..."
            "${staging_ca_script}" "${app_ns}" || warn "gentian-staging-ca-tls creation failed (tenant apps may not trust id.${KERNEL_DOMAIN})."
        fi
    fi
}
