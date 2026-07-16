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
    banner "Step 16 — AppCatalogue CRD + kubectl-gentian plugin"

    kubectl apply -f "${SCRIPT_DIR}/config/crd/gentianos.io_appcatalogues.yaml"
    success "AppCatalogue CRD applied."

    local plugin_src="${SCRIPT_DIR}/scripts/kubectl-gentian"
    local plugin_dst="/usr/local/bin/kubectl-gentian"
    local gtnctl_dst="/usr/local/bin/gtnctl"

    # Idempotency: skip if destination is identical to source (no sudo needed).
    if [[ -f "$plugin_dst" ]] && cmp -s "$plugin_src" "$plugin_dst"; then
        success "kubectl-gentian already up-to-date at ${plugin_dst}."
    elif [[ -w /usr/local/bin ]]; then
        install -m 755 "$plugin_src" "$plugin_dst"
        success "kubectl-gentian installed to ${plugin_dst}."
    else
        info "Installing kubectl-gentian to ${plugin_dst} (sudo required)..."
        if sudo install -m 755 "$plugin_src" "$plugin_dst"; then
            success "kubectl-gentian installed to ${plugin_dst}."
        else
            warn "Failed to install kubectl-gentian — install manually:"
            warn "  sudo install -m 755 ${plugin_src} ${plugin_dst}"
        fi
    fi

    if [[ ! -x "${plugin_dst}" ]]; then
        warn "Skipping gtnctl symlink — kubectl-gentian is not installed."
        return 0
    fi

    if [[ -w /usr/local/bin ]]; then
        ln -sf kubectl-gentian "${gtnctl_dst}"
    else
        sudo ln -sf kubectl-gentian "${gtnctl_dst}" || warn "Failed to link gtnctl -> kubectl-gentian at ${gtnctl_dst}"
    fi

    # Mirror to ~/.local/bin when present — it often precedes /usr/local/bin in PATH.
    local user_bin="${HOME}/.local/bin"
    local user_plugin_dst="${user_bin}/kubectl-gentian"
    local user_gtnctl_dst="${user_bin}/gtnctl"
    if [[ -d "${user_bin}" ]]; then
        if [[ -w "${user_bin}" ]]; then
            install -m 755 "$plugin_src" "$user_plugin_dst"
            ln -sf kubectl-gentian "${user_gtnctl_dst}"
            success "kubectl-gentian and gtnctl (-> kubectl-gentian) installed to ${user_bin}."
        else
            warn "${user_bin} is not writable — run: make -C ${SCRIPT_DIR} install-plugin"
        fi
    fi
}

# =============================================================================
# 15. Install Argo CD ApplicationSet syncing catalogue bundles from gentian-apps
# =============================================================================
# Renders kernel/bootstrap/catalogue-applicationset.yaml.tmpl. Once synced, each
# profiles/<name>/ kustomization becomes an Application (catalogue-<name>) that
# applies AppProfile, optional composition.yaml, and optional cluster assets.
install_catalogue_sync() {
    banner "Step 15 — Argo CD catalogue sync (gentian-apps profile bundles)"

    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/catalogue-applicationset.yaml.tmpl"
    local rendered
    rendered="$(mktemp)"
    sed -e "s|%REPO_URL%|${GENTIAN_APPS_REPO}|g" \
        -e "s|%BRANCH%|${GENTIAN_APPS_BRANCH}|g" \
        "$tmpl" >"$rendered"

    info "Applying gentian-catalogue ApplicationSet:"
    info "  repo:   ${GENTIAN_APPS_REPO}"
    info "  branch: ${GENTIAN_APPS_BRANCH}"
    kubectl apply -f "$rendered"
    rm -f "$rendered"
    success "Catalogue sync configured. Argo CD will sync profiles/<name>/ bundles."
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
#     kernel/bootstrap/gentian-os-application.yaml.tmpl) is applied.
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

# =============================================================================
# Create git credentials Secret for operator app lifecycle (gentian-deployments push)
# =============================================================================
_deployments_git_host() {
    local repo="${GENTIAN_DEPLOYMENTS_REPO:-https://git.example.domain/gentian-deployments}"
    if [[ "${repo}" =~ ^https?://([^/]+) ]]; then
        echo "${BASH_REMATCH[1]}"
    elif [[ "${repo}" =~ ^git@([^:]+): ]]; then
        echo "${BASH_REMATCH[1]}"
    else
        echo "github.com"
    fi
}

create_deployments_git_credentials() {
    local ns="${1:-gentian-system}"
    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        warn "GENTIAN_DEPLOYMENTS_GIT_TOKEN not set — skipping deployments git credentials Secret."
        warn "  In-cluster App Store install/uninstall will fail at git push until configured."
        return 0
    fi

    banner "Deployments git credentials (operator app lifecycle)"
    local host username
    host="$(_deployments_git_host)"
    username="${GENTIAN_DEPLOYMENTS_GIT_USERNAME:-x-access-token}"
    bash "${SCRIPT_DIR}/scripts/create-deployments-git-credentials.sh" \
        "${ns}" \
        "${GENTIAN_DEPLOYMENTS_GIT_TOKEN}" \
        "${username}" \
        "${host}"
    success "Deployments git credentials Secret ready in ${ns}."
}

# =============================================================================
# Upload CI_BOT_PAT (and optional ArgoCD sync secrets) to gentian-os GitHub repo
# =============================================================================
configure_github_actions_secrets() {
    if [[ -z "${CI_BOT_PAT:-}" ]]; then
        warn "CI_BOT_PAT not set — skipping GitHub Actions secret upload for image-pin workflows."
        warn "  CI_BOT_PAT not configured — gentian-os git automation from Actions may fail."
        warn "  See getting-started.md § GitHub Actions CI and gentian-ui/docs/ci-setup.md."
        return 0
    fi

    banner "GitHub Actions secrets (gentian-os image pin)"
    bash "${SCRIPT_DIR}/scripts/configure-github-actions-secrets.sh"
}

# Adopt cluster-scoped chart resources left from a prior ArgoCD or manual install so
# helm upgrade --install gentian-os can proceed (missing meta.helm.sh/release-*).
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
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
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
    # Application (kernel/bootstrap/gentian-os-application.yaml.tmpl):
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
    echo "gentian-${ENV:-dev}"
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
    local gentian_os_branch
    gentian_os_branch=$(git -C "${SCRIPT_DIR}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "develop")
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-${ENV:-dev}}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER:-default-cluster}"
    local tmpl="${SCRIPT_DIR}/kernel/bootstrap/gentian-os-application.yaml.tmpl"
    local rendered
    rendered="$(mktemp)"
    sed -e "s|%GENTIAN_OS_BRANCH%|${gentian_os_branch}|g" \
        -e "s|%DEPLOYMENTS_REPO%|${GENTIAN_DEPLOYMENTS_REPO}|g" \
        -e "s|%DEPLOYMENTS_BRANCH%|${GENTIAN_DEPLOYMENTS_BRANCH}|g" \
        -e "s|%CLUSTER%|${cluster}|g" \
        -e "s|%STAGE%|${stage}|g" \
        "$tmpl" >"$rendered"

    info "Registering gentian-os Application + gentian-tenants ApplicationSet..."
    info "  operator branch:    ${gentian_os_branch}"
    info "  deployments repo:   ${GENTIAN_DEPLOYMENTS_REPO}"
    info "  deployments branch: ${GENTIAN_DEPLOYMENTS_BRANCH}"
    info "  deployments cluster:${cluster}"
    info "  deployments stage:  ${stage}"
    kubectl apply -f "$rendered"
    rm -f "$rendered"

    if [[ -n "${openfga_token}" ]]; then
        info "Pinning Stage 1 authz bridge settings on Argo CD Application gentian-os..."
        kubectl patch application gentian-os -n argocd --type=json -p "$(jq -nc \
            --arg token "${openfga_token}" \
            '[{"op":"add","path":"/spec/sources/0/helm/parameters","value":[
                {"name":"authzBridge.enabled","value":"true"},
                {"name":"authzBridge.openfgaToken","value":$token}
            ]}]')" 2>/dev/null \
        || kubectl patch application gentian-os -n argocd --type=json -p "$(jq -nc \
            --arg token "${openfga_token}" \
            '[{"op":"replace","path":"/spec/sources/0/helm/parameters","value":[
                {"name":"authzBridge.enabled","value":"true"},
                {"name":"authzBridge.openfgaToken","value":$token}
            ]}]')"
    fi

    release_gentian_os_helm_bootstrap "gentian-system"
    kubectl annotate application gentian-os -n argocd \
        "argocd.argoproj.io/refresh=hard" --overwrite >/dev/null 2>&1 || true

    success "ArgoCD handoff complete: gentian-os Application and gentian-tenants ApplicationSet registered."
    success "  Image updates are now fully automatic via argocd-image-updater."
    info "Monitor operator:  kubectl get application gentian-os -n argocd"
    info "Monitor tenants:   kubectl get applicationset gentian-tenants -n argocd"
}

install_gentian_os_operator() {
    banner "Step 13 — gentian-os operator (Stage 1 authz bridge + Cloudflare tunnel)"

    local chart_dir="${SCRIPT_DIR}/charts/gentian-os"
    local crd_dir="${chart_dir}/crds"
    local ns="gentian-system"

    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl create namespace "$ns"
    fi

    create_deployments_git_credentials "$ns"

    local openfga_token=""
    if kubectl get secret openfga-sensitive-values -n platform-kernel >/dev/null 2>&1; then
        openfga_token=$(kubectl get secret openfga-sensitive-values -n platform-kernel \
            -o jsonpath='{.data.sensitive-values\.yaml}' 2>/dev/null | base64 -d 2>/dev/null \
            | grep -A1 'keys:' | tail -1 | sed 's/.*"\([^"]*\)".*/\1/' || true)
    fi

    local value_files=()
    _gentian_os_collect_operator_value_files value_files

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
        --set "authzBridge.openfgaToken=${openfga_token}" \
        --set infraNamespace="${_infra_ns}" \
        --set "kernelServices.keycloakInternalURL=http://gentian-idp-keycloak-keycloakx-http.platform-kernel.svc.cluster.local:8080/auth" \
        ${OPENPROJECT_ENTERPRISE_TOKEN:+--set "kernelServices.openprojectEnterpriseToken=${OPENPROJECT_ENTERPRISE_TOKEN}"} \
        --set "image.tag=${operator_tag}" \
        --set "image.pullPolicy=${GENTIAN_OS_IMAGE_PULL_POLICY:-Always}" \
        --wait --timeout 5m

    wait_for_operator_cloudflare_token "$ns" || true

    handoff_gentian_os_to_argocd "${openfga_token}"

    success "gentian-os operator installed with AUTHZ_BRIDGE_ENABLED and Cloudflare tunnel wiring."
    info "OpenFGA runtime secret: kubectl get secret openfga-runtime -n platform-kernel"
    info "Operator Cloudflare:    kubectl logs -n gentian-system deploy/gentian-os | grep -i cloudflare"
}
