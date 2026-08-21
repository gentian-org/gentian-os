#!/usr/bin/env bash
# =============================================================================
# scripts/lib/catalogue.sh — App catalogue sync, operator Helm bootstrap, and orchestrator handoff.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# 16. AppCatalogue CRD + kubectl-gentian plugin
# =============================================================================
install_app_catalogue() {
    banner "AppCatalogue CRD"

    # The CRD is not applied here.
    #
    # It used to be, from config/crd/ in this checkout — a file byte-identical to
    # charts/gentian-os/crds/gentianos.io_appcatalogues.yaml, which the
    # gentian-os Argo CD Application delivers with the operator chart at
    # sync-wave 0. Two writers, one of them whatever tree the installer happened
    # to run from.
    #
    # Nothing local populates the catalogue either: AppProfiles arrive from
    # gentian-apps through the gentian-catalogue ApplicationSet, and the
    # operator's appstore controller creates the AppCatalogue singleton and
    # rebuilds its status from them. So this step waits for what Argo brings.
    local deadline=$((SECONDS + 120))
    until kubectl get crd appcatalogues.gentianos.io >/dev/null 2>&1; do
        if (( SECONDS > deadline )); then
            warn "AppCatalogue CRD not present after 2m — check the gentian-os Application in Argo CD."
            return 0
        fi
        sleep 5
    done
    success "AppCatalogue CRD is present (delivered by the operator chart)."

    # The host CLI is NOT installed from here. Installing it meant a cluster
    # installer asking for root on the machine it was run from, and an uninstall
    # deleting the binary that drives every other cluster the operator manages.
    # It is a host operation, so it has a host command.
    if ! command -v kubectl-gentian >/dev/null 2>&1; then
        info "The gentian CLI is not on PATH. Install it with:"
        info "  make -C ${SCRIPT_DIR} install-plugin"
    fi
}

# Every host path install_app_catalogue writes: the plugin and its gtnctl
# symlink, in /usr/local/bin and in the ~/.local/bin mirror.
#
# Both destinations are real. ~/.local/bin usually precedes /usr/local/bin in
# PATH, so removing only the system copy leaves `gtnctl` still resolving to a
# plugin for a cluster that no longer exists.

# =============================================================================
# 15. Repository claim for the default app catalogue (gentian-apps)
# =============================================================================
# The gentian-apps Repository claim, not a hand-authored ApplicationSet: any
# role: apps, type: git repository composes its own catalogue-sync
# ApplicationSet (crossplane/compositions/repository-default.yaml), so the
# cluster's default catalogue works the same way a tenant's private one does.
# Once synced, each profiles/<name>/ bundle becomes an Application
# (catalogue-<name>) that applies AppProfile, optional composition.yaml, and
# optional cluster assets.
#
# Applied here rather than through the gentian-claims ApplicationSet because
# GENTIAN_APPS_REPO/GENTIAN_APPS_BRANCH are install-time configuration a
# cluster may override, and every cluster gets the default catalogue without
# the deployments repository having to commit anything for it — the same
# reason B-09 applies the deployments Repository claim directly rather than
# through Git.
install_catalogue_sync() {
    banner "App catalogue (gentian-apps Repository claim)"

    info "Applying Repository claim gentian-apps:"
    info "  repo:   ${GENTIAN_APPS_REPO}"
    info "  branch: ${GENTIAN_APPS_BRANCH}"
    [[ "${GENTIAN_DRY_RUN:-0}" == "1" ]] && return 0

    kubectl apply -f - <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: gentian-apps
  namespace: crossplane-system
spec:
  type: git
  role: apps
  endpoints:
    inCluster: ${GENTIAN_APPS_REPO}
  branch: ${GENTIAN_APPS_BRANCH}
EOF
    success "Repository claim applied. Argo CD will sync profiles/<name>/ bundles."
    info "After sync, list available app profiles with:"
    info "  kubectl gentian apps list"
}

# =============================================================================
# 15. Install gentian-os orchestrator (Helm chart + ArgoCD Application)
# =============================================================================
# The orchestrator chart at charts/gentian-os/ ships:
#   - CRDs: tenants, appprofiles, integrationbindings, appcatalogues
#   - Deployment + ServiceAccount + ClusterRole(Binding) for the operator
#   - ServiceMonitor + Grafana dashboard
#
# Two-step install:
#   Direct Helm bootstrap (fast):
#     CRDs and the operator Deployment are applied immediately so that
#     subsequent install steps can use them without waiting for ArgoCD.
#   ArgoCD Application handoff:
#     The gentian-os ArgoCD Application (rendered from
#     kernel/bootstrap/chart/templates/gentian-os.yaml) is applied.
#     ArgoCD takes ownership of the resources via ServerSideApply and from
#     this point drives all future chart upgrades.  Critically, Source 4 of
#     the Application deploys the ImageUpdater CR into the cluster, which
#     activates argocd-image-updater's automatic image rollout: whenever a
#     new image is pushed to GHCR for the tracked branch, argocd-image-updater
#     patches image.tag in the Application's Helm parameters and ArgoCD
#     triggers a Helm upgrade (rolling restart) automatically.
#
# Without the ArgoCD handoff, argocd-image-updater reports "no ImageUpdater CRs to
# process" and image updates require manual kubectl rollout restart.
# =============================================================================
release_gentian_os_helm_bootstrap() {
    local ns="${1:-gentian-system}"
    if kubectl get secret -n "$ns" -l "owner=helm,name=gentian-os" --no-headers 2>/dev/null | grep -q .; then
        info "Removing bootstrap Helm release metadata (ArgoCD owns gentian-os now)..."
        kubectl delete secret -n "$ns" -l "owner=helm,name=gentian-os" --ignore-not-found
    fi
}

# The operator's .git-credentials used to be created here with `kubectl create
# secret`, alongside the one the XRepository Composition emits through ESO —
# two writers of one credential, and the values file decided which the operator
# mounted. The composed one is the credential: it is backed by an
# ExternalSecret, so rotating the value in OpenBao reaches the pod, which the
# imperative Secret could never do.
#
# It survived this long because the Composition named it from the composite
# (deployments-m288c-git-credentials), and a chart value cannot be written
# against a generated suffix. The Composition now names it from the claim, so
# `deployments-git-credentials` is stable and referenceable.

# Adopt cluster-scoped chart resources left from a prior ArgoCD or manual install so
# helm upgrade --install gentian-os can proceed (missing meta.helm.sh/release-*).
# =============================================================================
# ensure_kernel_services_configmap — break the Step 12 / Step 13 deadlock
#
# The Keycloak pod created by the Suze XR (Step 12) reads KERNEL_DOMAIN from the
# gentian-kernel-services ConfigMap in platform-kernel. That ConfigMap is
# rendered by the gentian-os operator chart — Step 13. So on a first install the
# pod fails with
#
#   Error: configmap "gentian-kernel-services" not found
#
# and Step 12 waits out its full 1200s timeout for a pod that cannot start,
# while the thing it needs is scheduled to arrive one step later. Re-runs of an
# already-bootstrapped cluster hide this, because the ConfigMap is left over
# from the previous run — which is why it survived until the first prod install.
#
# Seed it before Step 12 with the keys that are unambiguous this early
# (KERNEL_DOMAIN, TENANCY_MODE — the only ones any Step 12 workload reads), and
# tag it so Helm adopts rather than collides at Step 13, using the same
# annotation/label pair as adopt_gentian_os_helm_preflight below. Step 13 then
# re-renders it with the full key set (SMTP_HOST, S3_ENDPOINT, MYSQL_HOST, …),
# which is deliberately NOT guessed here: those hostnames come from chart
# defaults that this function has no business duplicating.
# =============================================================================
ensure_kernel_services_configmap() {
    local ns="platform-kernel"

    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        warn "KERNEL_DOMAIN unset; skipping gentian-kernel-services pre-seed."
        return 0
    fi

    kubectl get namespace "${ns}" >/dev/null 2>&1 || kubectl create namespace "${ns}" >/dev/null

    if kubectl get configmap gentian-kernel-services -n "${ns}" >/dev/null 2>&1; then
        success "gentian-kernel-services already present in ${ns}."
        return 0
    fi

    info "Pre-seeding gentian-kernel-services in ${ns} (needed by Keycloak in Step 12)..."
    kubectl create configmap gentian-kernel-services -n "${ns}" \
        --from-literal=KERNEL_DOMAIN="${KERNEL_DOMAIN}" \
        --from-literal=TENANCY_MODE="${TENANCY_MODE:-multi}" >/dev/null

    # Same adoption contract as adopt_gentian_os_helm_preflight: without these,
    # Step 13's `helm upgrade --install` aborts with "invalid ownership metadata".
    kubectl annotate configmap gentian-kernel-services -n "${ns}" \
        "meta.helm.sh/release-name=gentian-os" \
        "meta.helm.sh/release-namespace=gentian-system" --overwrite >/dev/null
    kubectl label configmap gentian-kernel-services -n "${ns}" \
        "app.kubernetes.io/managed-by=Helm" \
        "gentianos.io/config-type=kernel-services" --overwrite >/dev/null

    success "gentian-kernel-services seeded (Helm will adopt it in Step 13)."
}

adopt_gentian_os_helm_preflight() {
    local ns="${1:-gentian-system}"
    local vwc chart_ns

    while IFS= read -r vwc; do
        [[ -z "$vwc" ]] && continue
        if kubectl get validatingwebhookconfiguration "$vwc" \
            -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null \
            | grep -q .; then
            continue
        fi
        kubectl annotate validatingwebhookconfiguration "$vwc" \
            "meta.helm.sh/release-name=gentian-os" \
            "meta.helm.sh/release-namespace=${ns}" \
            --overwrite
        kubectl label validatingwebhookconfiguration "$vwc" \
            "app.kubernetes.io/managed-by=Helm" \
            --overwrite
        info "Adopted pre-existing ValidatingWebhookConfiguration '${vwc}' into Helm release."
    done < <(kubectl get validatingwebhookconfigurations \
                 --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
             | grep "^gentian-os-" || true)

    chart_ns="shared-apps"
    if kubectl get namespace "${chart_ns}" >/dev/null 2>&1; then
        if ! kubectl get namespace "${chart_ns}" \
            -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null \
            | grep -q .; then
            kubectl annotate namespace "${chart_ns}" \
                "meta.helm.sh/release-name=gentian-os" \
                "meta.helm.sh/release-namespace=${ns}" \
                --overwrite
            kubectl label namespace "${chart_ns}" \
                "app.kubernetes.io/managed-by=Helm" \
                --overwrite
            info "Adopted pre-existing namespace '${chart_ns}' into Helm release."
        fi
    fi
}

_gentian_os_deployments_kernel_dir() {
    : "${GENTIAN_DEPLOYMENTS_PATH:=${HOME}/.gentian/gentian-deployments}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
    echo "${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel"
}

_gentian_os_collect_operator_value_files() {
    local -n _files=$1
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local kernel_dir deploy_dir
    kernel_dir="$(_gentian_os_deployments_kernel_dir)"
    deploy_dir="${GENTIAN_DEPLOYMENTS_PATH:-${HOME}/.gentian/gentian-deployments}"
    _files=()
    # Mirrors the layered valueFiles ArgoCD uses once it takes over this
    # Application (kernel/bootstrap/chart/templates/gentian-os.yaml):
    # profiles/_base.yaml -> profiles/<stage>.yaml -> clusters/<cluster>/kernel/values.yaml
    if [[ -f "${deploy_dir}/profiles/_base.yaml" ]]; then
        _files+=(-f "${deploy_dir}/profiles/_base.yaml")
    else
        warn "Missing ${deploy_dir}/profiles/_base.yaml — operator shared defaults not applied."
    fi
    if [[ -f "${deploy_dir}/profiles/${stage}.yaml" ]]; then
        _files+=(-f "${deploy_dir}/profiles/${stage}.yaml")
        info "Layering operator Helm values from gentian-deployments (profiles/${stage}.yaml)."
    else
        warn "Missing ${deploy_dir}/profiles/${stage}.yaml — operator stage overlay not applied."
    fi
    if [[ -f "${kernel_dir}/values.yaml" ]]; then
        _files+=(-f "${kernel_dir}/values.yaml")
        info "Layering operator Helm values from gentian-deployments (clusters/.../kernel/values.yaml)."
    else
        warn "Missing ${kernel_dir}/values.yaml — operator Cloudflare/DNS settings may be incomplete."
    fi
}

_gentian_os_services_namespace() {
    local ns
    ns=$(kubectl get deploy gentian-os -n gentian-system \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="SERVICES_NAMESPACE")].value}' 2>/dev/null || true)
    if [[ -n "$ns" ]]; then
        echo "$ns"
        return
    fi
    # Fallback must match the chart default, not the old gentian-<env> guess.
    gentian_services_namespace
}

wait_for_operator_cloudflare_token() {
    local ns="${1:-gentian-system}"
    local secret_name="${2:-cloudflare-api-token-gentian-system}"
    if ! kubectl get externalsecret "${secret_name}" -n "${ns}" >/dev/null 2>&1; then
        info "No operator Cloudflare ExternalSecret in ${ns}; skipping API token wait."
        return 0
    fi
    info "Waiting for operator Cloudflare API token Secret ${secret_name} (max 120s)..."
    local i
    for (( i=1; i<=60; i++ )); do
        if kubectl get secret "${secret_name}" -n "${ns}" >/dev/null 2>&1; then
            success "Operator Cloudflare API token ready after $((i * 2))s."
            return 0
        fi
        sleep 2
    done
    warn "Secret ${secret_name} did not materialize within 120s."
    warn "  kubectl describe externalsecret ${secret_name} -n ${ns}"
    warn "  Ensure CF_API_TOKEN was seeded (Step 10c) and OpenBao path gentian-os/kernel/dns/cloudflare exists."
    return 1
}

handoff_gentian_os_to_argocd() {
    local openfga_token="${1:-}"
    # Passed explicitly rather than relied on through bash's dynamic scoping,
    # which would silently resolve to the caller's local and break the moment
    # this is called from anywhere else.
    local openfga_token_in_bao="${2:-0}"
    resolve_gentian_os_branch
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-default-cluster}"
    # Provenance defaults to the public origin, so an install that says nothing
    # behaves as before; a mirrored install sets these in install.env (§2).
    local os_repo="${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}"
    local os_image="${GENTIAN_OS_IMAGE_REPOSITORY:-ghcr.io/gentian-org/gentian-os}"
    local rendered
    rendered="$(mktemp)"
    # The ApplicationSet in this manifest carries Argo's own {{ .path.path }},
    # which the chart wraps in a raw string so Helm hands it through untouched.
    helm template gentian-bootstrap "${SCRIPT_DIR}/kernel/bootstrap/chart" \
        -s templates/gentian-os.yaml \
        --set-string "gentianOsBranch=${GENTIAN_OS_BRANCH}" \
        --set-string "osRepo=${os_repo}" \
        --set-string "osImageRepository=${os_image}" \
        --set-string "deploymentsRepo=${GENTIAN_DEPLOYMENTS_REPO}" \
        --set-string "deploymentsBranch=${GENTIAN_DEPLOYMENTS_BRANCH}" \
        --set-string "cluster=${cluster}" \
        --set-string "stage=${stage}" >"$rendered"

    info "Registering gentian-os Application + gentian-tenants ApplicationSet..."
    info "  operator branch:    ${GENTIAN_OS_BRANCH}"
    info "  deployments repo:   ${GENTIAN_DEPLOYMENTS_REPO}"
    info "  deployments branch: ${GENTIAN_DEPLOYMENTS_BRANCH}"
    info "  operator repo:      ${os_repo}"
    info "  operator image:     ${os_image}"
    info "  deployments cluster:${cluster}"
    info "  deployments stage:  ${stage}"
    kubectl apply -f "$rendered"
    rm -f "$rendered"

    # Pin only non-sensitive bridge settings. The OpenFGA token deliberately does
    # NOT go here: helm.parameters are stored verbatim in the Application spec,
    # so writing it pinned the credential in cleartext where every reader of
    # Applications — and every backup or git mirror of them — could see it. The
    # chart now resolves the token through secretKeyRef, so the Application only
    # needs to say which Secret to read.
    local -a app_params=('{"name":"authzBridge.enabled","value":"true"}')

    # The trust anchor, when a public CA did not sign this cluster's
    # certificates. Read from the claim rather than asked for again: issuerMode
    # already decides whether an anchor exists, and a second setting would be
    # free to disagree with the mode that produced it. Non-sensitive — a CA
    # certificate is the thing you hand to strangers — so pinning the NAME here
    # is safe in a way the OpenFGA token above is not.
    local _issuer_mode _anchor_secret
    _issuer_mode="$(kubectl get clusters.gentianos.io \
        -o jsonpath='{.items[0].spec.certificates.issuerMode}' 2>/dev/null || true)"
    case "${_issuer_mode}" in
        self-signed|private-ca)
            _anchor_secret="$(kubectl get clusters.gentianos.io \
                -o jsonpath='{.items[0].spec.certificates.caBundleSecretRef.name}' 2>/dev/null || true)"
            _anchor_secret="${_anchor_secret:-gentian-root-ca-tls}"
            app_params+=("{\"name\":\"trustAnchorSecret\",\"value\":\"${_anchor_secret}\"}")
            info "  trust anchor:       ${_anchor_secret} (issuerMode=${_issuer_mode})"
            ;;
    esac
    if ((openfga_token_in_bao)); then
        app_params+=('{"name":"authzBridge.openfgaTokenSecretRef.name","value":"gentian-os-openfga-token"}')
    elif [[ -n "${openfga_token}" ]]; then
        warn "Pinning the OpenFGA token into the Argo CD Application in cleartext"
        warn "  because it could not be stored in OpenBao. Re-run once OpenBao is"
        warn "  reachable, then rotate the token — the old value stays in any"
        warn "  backup of the Application taken meanwhile."
        app_params+=("$(jq -nc --arg t "${openfga_token}" \
            '{"name":"authzBridge.openfgaToken","value":$t}')")
    fi

    info "Pinning Stage 1 authz bridge settings on Argo CD Application gentian-os..."
    local params_json
    params_json=$(printf '%s\n' "${app_params[@]}" | jq -sc .)
    kubectl patch application gentian-os -n argocd --type=json -p \
        "$(jq -nc --argjson v "${params_json}" \
            '[{"op":"add","path":"/spec/sources/0/helm/parameters","value":$v}]')" 2>/dev/null \
    || kubectl patch application gentian-os -n argocd --type=json -p \
        "$(jq -nc --argjson v "${params_json}" \
            '[{"op":"replace","path":"/spec/sources/0/helm/parameters","value":$v}]')"

    release_gentian_os_helm_bootstrap "gentian-system"
    kubectl annotate application gentian-os -n argocd \
        "argocd.argoproj.io/refresh=hard" --overwrite >/dev/null 2>&1 || true

    success "ArgoCD handoff complete: gentian-os Application and gentian-tenants ApplicationSet registered."
    success "  Image updates are now fully automatic via argocd-image-updater."
    info "Monitor operator:  kubectl get application gentian-os -n argocd"
    info "Monitor tenants:   kubectl get applicationset gentian-tenants -n argocd"
}

install_gentian_os_operator() {
    banner "gentian-os operator (Stage 1 authz bridge + Cloudflare tunnel)"

    local chart_dir="${SCRIPT_DIR}/charts/gentian-os"
    local crd_dir="${chart_dir}/crds"
    local ns="gentian-system"

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    # The OpenFGA token is not written here. B-06 derives it and writes
    # gentian-os/kernel/authz/openfga with the field `preshared_key`, which is
    # the name OpenFGA's own ExternalSecret reads, and the chart's ExternalSecret
    # now reads the same field. Writing it again from this side re-derived the
    # value out of a Kubernetes Secret by grepping a YAML blob, and stored it
    # under a second field name — and because a KV v2 write replaces the whole
    # secret version rather than merging, the later of the two writers silently
    # removed the other's field. Whichever ran last decided whether the operator
    # or OpenFGA itself lost its token.
    local openfga_token="" openfga_token_in_bao=1

    # When the token reached OpenBao the chart's ExternalSecret supplies it and
    # nothing token-shaped crosses the Helm boundary — the chart's default
    # secretRef already names the right Secret, so there is nothing to set.
    # Otherwise pass the literal, which the chart prefers when present.
    local -a authz_token_args=()
    if ((openfga_token_in_bao)); then
        authz_token_args+=(--set "authzBridge.openfgaTokenSecretRef.name=gentian-os-openfga-token")
    # An explicit if, not `[[ ... ]] && cmd`: under set -e that idiom aborts the
    # install when the token is empty, because the && list would be the last
    # command in the branch and its status becomes the branch's.
    elif [[ -n "${openfga_token}" ]]; then
        authz_token_args+=(--set "authzBridge.openfgaToken=${openfga_token}")
    fi

    local value_files=()
    _gentian_os_collect_operator_value_files value_files

    # Cluster-supplied kernel-service passthrough. Any GENTIAN_KERNEL_SERVICE_<KEY>
    # in the environment is published as <KEY> in the gentian-kernel-services
    # ConfigMap, so an app profile that needs a cluster-provided value (a licence
    # token, a third-party endpoint) can get one without the installer or the
    # chart carrying a field named after that app.
    local -a kernel_service_extra_args=()
    local _ks_var _ks_key
    for _ks_var in "${!GENTIAN_KERNEL_SERVICE_@}"; do
        _ks_key="${_ks_var#GENTIAN_KERNEL_SERVICE_}"
        [[ -z "${_ks_key}" || -z "${!_ks_var}" ]] && continue
        # --set-string, and the key escaped: Helm reads "." and "," inside a --set
        # path as structure, so an unescaped key with either would silently create
        # nested maps instead of one ConfigMap entry.
        kernel_service_extra_args+=(--set-string "kernelServices.extra.${_ks_key//./\\.}=${!_ks_var}")
    done

    if [[ ! -d "$crd_dir" ]]; then
        error "CRD directory not found: ${crd_dir}"
        exit 1
    fi
    kubectl apply -f "$crd_dir"
    adopt_gentian_os_helm_preflight "$ns"

    local operator_tag="${GENTIAN_OS_IMAGE_TAG:-develop}"
    local _infra_ns="${INFRA_NAMESPACE:-gentian-infra-${ENV:-dev}}"
    info "Using gentian-os operator image ghcr.io/gentian-org/gentian-os:${operator_tag} (CI)."

    helm upgrade --install gentian-os "$chart_dir" \
        --namespace "$ns" \
        "${value_files[@]}" \
        --set openbao.address="https://openbao.openbao.svc.cluster.local:8200" \
        --set kernelDomain="${KERNEL_DOMAIN}" \
        --set tenancyMode="${TENANCY_MODE:-multi}" \
        --set routingMode="${ROUTING_MODE:-gateway}" \
        --set kernelRealm="${KERNEL_REALM:-kernel}" \
        --set authzBridge.enabled=true \
        --set authzBridge.openfgaURL="http://gentian-openfga.platform-kernel.svc.cluster.local:8080" \
        "${authz_token_args[@]}" \
        --set infraNamespace="${_infra_ns}" \
        --set "kernelServices.keycloakInternalURL=http://gentian-idp-keycloak-keycloakx-http.platform-kernel.svc.cluster.local:8080/auth" \
        "${kernel_service_extra_args[@]}" \
        --set "image.tag=${operator_tag}" \
        --set "image.pullPolicy=${GENTIAN_OS_IMAGE_PULL_POLICY:-Always}" \
        --wait --timeout 5m

    wait_for_operator_cloudflare_token "$ns" || true

    handoff_gentian_os_to_argocd "${openfga_token}" "${openfga_token_in_bao}"

    success "gentian-os operator installed with AUTHZ_BRIDGE_ENABLED and Cloudflare tunnel wiring."
    info "OpenFGA runtime secret: kubectl get secret openfga-runtime -n platform-kernel"
    info "Operator Cloudflare:    kubectl logs -n gentian-system deploy/gentian-os | grep -i cloudflare"
}
