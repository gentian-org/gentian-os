#!/usr/bin/env bash
# =============================================================================
# install.sh — Fresh-cluster bootstrap for Gentian OS
# =============================================================================
# This script is fully self-contained. It depends only on files inside this
# repository (gentian-os) plus the user-supplied secrets listed below. No
# reference to any other Gentian repository is made.
#
# It installs and configures every kernel component in the correct order:
#    1. CLI tools (tofu, bao)
#    2. Kubernetes namespaces
#    3. External Secrets Operator (ESO) via Helm
#    4. ArgoCD + gentian AppProject
#    5. ArgoCD OCI registry secrets
#    6. OpenBao transit seal instance
#    7. Transit init + autounseal Secret
#    8. Remaining ArgoCD bootstrap Applications (openbao, tofu-controller, globals)
#    9. Primary OpenBao init (transit auto-unseal)
#   10. OpenBao configuration via Tofu (KV engine, K8s auth, policies, operator role)
#   11. Seed kernel secrets (scripts/seed-openbao.sh)
#   12. Apply root ApplicationSet → ArgoCD syncs the full stack
#   13. Install AppCatalogue CRD + kubectl-gentian plugin
#
# Required environment variables (prompted interactively if not pre-exported):
#   MASTER_PASSWORD                — master password for HKDF-derived secrets
#   OD_PRIVATE_REGISTRY_USERNAME   — registry.opencode.de username
#   OD_PRIVATE_REGISTRY_PASSWORD   — registry.opencode.de password or token
#   OD_SMTP_RELAY_USERNAME         — SMTP relay username (e.g. Gmail address)
#   OD_SMTP_RELAY_PASSWORD         — SMTP relay password (e.g. Gmail App Password)
#
# Optional environment variables:
#   NODE_IP           — cluster node IP (default: auto-detected)
#   SKIP_TOOLS        — set to "1" to skip CLI tool installation
#   OPENBAO_INIT_FILE — path to save OpenBao init keys (default: /tmp/openbao-init.json)
#
# Usage:
#   ./install.sh
# =============================================================================

set -euo pipefail

# ─── Colour helpers ──────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
banner()  { echo -e "\n${CYAN}══════════════════════════════════════════════════${NC}"; echo -e "${CYAN}  $*${NC}"; echo -e "${CYAN}══════════════════════════════════════════════════${NC}\n"; }

# ─── Paths ────────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ─── Runtime defaults ─────────────────────────────────────────────────────────
OPENBAO_INIT_FILE="${OPENBAO_INIT_FILE:-/tmp/openbao-init.json}"

# ─── Versions ────────────────────────────────────────────────────────────────
TOFU_VERSION="1.9.0"
BAO_VERSION="2.5.1"

# =============================================================================
# Prompt for any required credentials that were not pre-exported
# =============================================================================
prompt_credentials() {
    local prompted=0

    if [[ -z "${MASTER_PASSWORD:-}" ]]; then
        read -rp "  MASTER_PASSWORD (HKDF master secret): " MASTER_PASSWORD; echo ""
        export MASTER_PASSWORD
        prompted=1
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_USERNAME:-}" ]]; then
        read -rp  "  OD_PRIVATE_REGISTRY_USERNAME (registry.opencode.de): " OD_PRIVATE_REGISTRY_USERNAME; echo ""
        export OD_PRIVATE_REGISTRY_USERNAME
        prompted=1
    fi
    if [[ -z "${OD_PRIVATE_REGISTRY_PASSWORD:-}" ]]; then
        read -rp "  OD_PRIVATE_REGISTRY_PASSWORD (registry.opencode.de token): " OD_PRIVATE_REGISTRY_PASSWORD; echo ""
        export OD_PRIVATE_REGISTRY_PASSWORD
        prompted=1
    fi
    if [[ -z "${OD_SMTP_RELAY_USERNAME:-}" ]]; then
        read -rp  "  OD_SMTP_RELAY_USERNAME (e.g. user@gmail.com): " OD_SMTP_RELAY_USERNAME; echo ""
        export OD_SMTP_RELAY_USERNAME
        prompted=1
    fi
    if [[ -z "${OD_SMTP_RELAY_PASSWORD:-}" ]]; then
        read -rp "  OD_SMTP_RELAY_PASSWORD (e.g. Gmail App Password): " OD_SMTP_RELAY_PASSWORD; echo ""
        export OD_SMTP_RELAY_PASSWORD
        prompted=1
    fi

    [[ "$prompted" -eq 1 ]] && echo ""
}

# =============================================================================
# 0. Pre-flight checks
# =============================================================================
check_prereqs() {
    banner "Pre-flight checks"

    local missing=0
    for cmd in kubectl helm jq openssl curl; do
        if ! command -v "$cmd" &>/dev/null; then
            error "Required command not found: $cmd"
            missing=$((missing + 1))
        else
            success "$cmd found"
        fi
    done

    for var in MASTER_PASSWORD OD_PRIVATE_REGISTRY_USERNAME OD_PRIVATE_REGISTRY_PASSWORD \
               OD_SMTP_RELAY_USERNAME OD_SMTP_RELAY_PASSWORD; do
        if [[ -z "${!var:-}" ]]; then
            error "$var is not set"
            missing=$((missing + 1))
        else
            success "$var set"
        fi
    done

    if [[ "$missing" -gt 0 ]]; then
        error "$missing prerequisite(s) missing. Aborting."
        exit 1
    fi

    if [[ -z "${NODE_IP:-}" ]]; then
        NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
        info "Auto-detected NODE_IP: $NODE_IP"
    fi
    export NODE_IP

    success "All pre-flight checks passed."
}

# =============================================================================
# 1. Install CLI tools
# =============================================================================
install_tools() {
    if [[ "${SKIP_TOOLS:-0}" == "1" ]]; then
        warn "SKIP_TOOLS=1 — skipping CLI tool installation."
        return
    fi

    banner "Step 1 — Installing CLI tools"

    if command -v tofu &>/dev/null && tofu version 2>/dev/null | grep -q "$TOFU_VERSION"; then
        success "tofu $TOFU_VERSION already installed."
    else
        info "Installing OpenTofu v${TOFU_VERSION}..."
        local arch; arch=$(uname -m); [[ "$arch" == "x86_64" ]] && arch="amd64"
        local pkg="tofu_${TOFU_VERSION}_linux_${arch}.deb"
        curl -fsSL "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/${pkg}" -o "/tmp/${pkg}"
        sudo dpkg -i "/tmp/${pkg}"
        rm -f "/tmp/${pkg}"
        success "tofu $TOFU_VERSION installed."
    fi

    if command -v bao &>/dev/null && bao version 2>/dev/null | grep -q "$BAO_VERSION"; then
        success "bao $BAO_VERSION already installed."
    else
        info "Installing OpenBao CLI v${BAO_VERSION}..."
        local arch; arch=$(uname -m); [[ "$arch" == "x86_64" ]] && arch="amd64"
        local pkg="openbao_${BAO_VERSION}_linux_${arch}.deb"
        curl -fsSL "https://github.com/openbao/openbao/releases/download/v${BAO_VERSION}/${pkg}" -o "/tmp/${pkg}"
        sudo dpkg -i "/tmp/${pkg}"
        rm -f "/tmp/${pkg}"
        success "bao $BAO_VERSION installed."
    fi
}

# =============================================================================
# 2. Create namespaces (idempotent)
# =============================================================================
create_namespaces() {
    banner "Step 2 — Creating namespaces"

    local namespaces=(openbao external-secrets argocd tofu-system gentian-system platform-kernel)
    for ns in "${namespaces[@]}"; do
        if kubectl get namespace "$ns" &>/dev/null; then
            success "Namespace $ns already exists."
        else
            kubectl create namespace "$ns"
            success "Namespace $ns created."
        fi
    done
}

# =============================================================================
# 3. Install External Secrets Operator via Helm
# =============================================================================
install_eso() {
    banner "Step 3 — Installing External Secrets Operator"

    if helm status external-secrets -n external-secrets &>/dev/null; then
        success "ESO already installed. Skipping."
        return
    fi

    helm repo add external-secrets https://charts.external-secrets.io --force-update
    helm repo update
    helm install external-secrets external-secrets/external-secrets \
        -n external-secrets \
        -f "${SCRIPT_DIR}/kernel/eso/values.yaml" \
        --wait --timeout 5m
    success "ESO installed."
}

# =============================================================================
# 4. Install ArgoCD + AppProject
# =============================================================================
install_argocd() {
    banner "Step 4 — Installing ArgoCD"

    if kubectl get deployment argocd-server -n argocd &>/dev/null; then
        success "ArgoCD already installed."
    else
        bash "${SCRIPT_DIR}/scripts/install-argocd.sh"
        success "ArgoCD installed."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/projects/gentian.yaml"
    success "AppProject applied."
}

# =============================================================================
# 5. Create ArgoCD OCI registry secrets
# =============================================================================
setup_argocd_repos() {
    banner "Step 5 — ArgoCD OCI registry secrets"
    bash "${SCRIPT_DIR}/scripts/create-argocd-oci-secrets.sh" \
        "$OD_PRIVATE_REGISTRY_USERNAME" \
        "$OD_PRIVATE_REGISTRY_PASSWORD"
    success "ArgoCD OCI secrets configured."
}

# =============================================================================
# 6. Deploy OpenBao transit seal instance
# =============================================================================
bootstrap_transit_app() {
    banner "Step 6 — OpenBao transit seal instance"

    if ! kubectl get secret openbao-transit-unseal -n openbao &>/dev/null; then
        kubectl create secret generic openbao-transit-unseal \
            -n openbao --from-literal=unseal-key=placeholder
        success "Placeholder openbao-transit-unseal secret created."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/openbao-transit-application.yaml"
    success "Applied openbao-transit-application.yaml"

    info "Waiting for openbao-transit pod to become Running (up to 5 min)..."
    until kubectl get pods -n openbao -l app.kubernetes.io/instance=openbao-transit \
            --field-selector=status.phase=Running 2>/dev/null | grep -q openbao-transit; do
        echo -n "."
        sleep 5
    done
    echo ""
    success "openbao-transit pod is Running."
}

# =============================================================================
# 7. Init the transit instance
# =============================================================================
init_openbao_transit() {
    banner "Step 7 — Transit instance init + autounseal Secret"
    bash "${SCRIPT_DIR}/scripts/init-openbao-transit.sh"
}

# =============================================================================
# 8. Apply remaining ArgoCD bootstrap Applications
# =============================================================================
bootstrap_argocd_apps() {
    banner "Step 8 — ArgoCD bootstrap Applications"

    for app in openbao tofu-controller globals; do
        kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/${app}-application.yaml"
        success "Applied ${app}-application.yaml"
    done

    info "Waiting for OpenBao pod to become Running (up to 5 min)..."
    until kubectl get pods -n openbao -l app.kubernetes.io/name=openbao \
            --field-selector=status.phase=Running 2>/dev/null | grep -q openbao; do
        echo -n "."
        sleep 5
    done
    echo ""
    success "OpenBao pod is Running."
}

# =============================================================================
# 9. Initialize primary OpenBao (transit auto-unseal)
# =============================================================================
init_openbao() {
    banner "Step 9 — OpenBao init"

    info "Waiting for openbao service (up to 2 min)..."
    local i=0
    until kubectl get svc openbao -n openbao &>/dev/null; do
        echo -n "."; sleep 5; i=$((i + 5))
        [[ $i -lt 120 ]] || { error "Timed out."; exit 1; }
    done
    echo ""

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    local BAO_HTTP="http://${BAO_SVC_IP}:8200"

    local init_status
    init_status=$(curl -sf "${BAO_HTTP}/v1/sys/init" | jq -r '.initialized')

    if [[ "$init_status" == "true" ]]; then
        success "OpenBao already initialized."
        local sealed seal_type
        sealed=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
        seal_type=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')
        if [[ "$sealed" == "true" && "$seal_type" == "transit" ]]; then
            warn "OpenBao sealed — waiting for transit auto-unseal..."
            sleep 15
            sealed=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.sealed')
            [[ "$sealed" == "true" ]] && { error "Auto-unseal failed."; exit 1; }
            success "Transit auto-unseal completed."
        fi
        return
    fi

    local seal_type_before
    seal_type_before=$(curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -r '.type')

    if [[ "$seal_type_before" == "transit" ]]; then
        info "Initializing OpenBao with transit seal (recovery_shares=1)..."
        local init_resp
        init_resp=$(curl -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"recovery_shares": 1, "recovery_threshold": 1}')

        echo "$init_resp" > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local recovery_key root_token
        recovery_key=$(echo "$init_resp" | jq -r '.recovery_keys_b64[0]')
        root_token=$(echo "$init_resp"   | jq -r '.root_token')

        echo ""
        echo -e "${RED}╔═══════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${RED}║  ⚠  SAVE THESE VALUES (password manager)                     ║${NC}"
        echo -e "${RED}╠═══════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${RED}║  Recovery Key : ${recovery_key}${NC}"
        echo -e "${RED}║  Root Token   : ${root_token}${NC}"
        echo -e "${RED}╚═══════════════════════════════════════════════════════════════╝${NC}"
        echo ""
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        export BAO_TOKEN="$root_token"

        info "Waiting for transit auto-unseal (up to 30 s)..."
        i=0
        until curl -sf "${BAO_HTTP}/v1/sys/seal-status" | jq -e '.sealed == false' >/dev/null 2>&1; do
            sleep 3; i=$((i + 3))
            [[ $i -lt 30 ]] || { error "Auto-unseal timed out."; exit 1; }
        done
        success "OpenBao initialized and auto-unsealed via transit."
    else
        info "Initializing OpenBao (1-of-1 Shamir — transit unavailable)..."
        local init_resp
        init_resp=$(curl -sf -X PUT "${BAO_HTTP}/v1/sys/init" \
            -H "Content-Type: application/json" \
            -d '{"secret_shares": 1, "secret_threshold": 1}')

        echo "$init_resp" > "${OPENBAO_INIT_FILE}"
        chmod 600 "${OPENBAO_INIT_FILE}"

        local unseal_key root_token
        unseal_key=$(echo "$init_resp" | jq -r '.keys_base64[0]')
        root_token=$(echo "$init_resp"  | jq -r '.root_token')

        echo ""
        echo -e "${RED}║  Unseal Key : ${unseal_key}${NC}"
        echo -e "${RED}║  Root Token : ${root_token}${NC}"
        read -rp "  Saved both values? [yes/no]: " confirmed
        [[ "$confirmed" == "yes" ]] || { error "Aborted."; exit 1; }

        curl -sf -X PUT "${BAO_HTTP}/v1/sys/unseal" \
            -H "Content-Type: application/json" \
            -d "{\"key\": \"${unseal_key}\"}" | jq .
        export BAO_TOKEN="$root_token"
        success "OpenBao initialized and unsealed (Shamir)."
    fi
}

# =============================================================================
# 10. Configure OpenBao via Tofu
# =============================================================================
run_tofu_openbao_init() {
    banner "Step 10 — OpenBao configuration via Tofu"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export VAULT_ADDR="http://${BAO_SVC_IP}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export VAULT_TOKEN="$BAO_TOKEN"

    pushd "${SCRIPT_DIR}/kernel/tofu/platform/openbao-init" > /dev/null
        tofu init -backend=false
        tofu apply -auto-approve
    popd > /dev/null
    success "OpenBao configured via Tofu."
}

# =============================================================================
# 11. Seed kernel secrets
# =============================================================================
seed_secrets() {
    banner "Step 11 — Seeding kernel secrets"

    local BAO_SVC_IP
    BAO_SVC_IP=$(kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}')
    export BAO_ADDR="http://${BAO_SVC_IP}:8200"

    if [[ -z "${BAO_TOKEN:-}" ]]; then
        if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
            BAO_TOKEN=$(jq -r '.root_token' "${OPENBAO_INIT_FILE}")
        else
            read -rp "  Enter OpenBao root token: " BAO_TOKEN; echo ""
        fi
    fi
    export BAO_TOKEN

    bash "${SCRIPT_DIR}/scripts/seed-openbao.sh" \
        "$MASTER_PASSWORD" \
        "$OD_PRIVATE_REGISTRY_USERNAME" \
        "$OD_PRIVATE_REGISTRY_PASSWORD" \
        "$OD_SMTP_RELAY_USERNAME" \
        "$OD_SMTP_RELAY_PASSWORD"
    success "All kernel secrets seeded."
}

# =============================================================================
# 12. Apply root ApplicationSet
# =============================================================================
bootstrap_root_appset() {
    banner "Step 12 — Applying root ApplicationSet"
    bash "${SCRIPT_DIR}/scripts/bootstrap.sh"
    success "Root ApplicationSet applied."
}

# =============================================================================
# 13. AppCatalogue CRD + kubectl-gentian plugin
# =============================================================================
install_app_catalogue() {
    banner "Step 13 — AppCatalogue CRD + kubectl-gentian plugin"

    kubectl apply -f "${SCRIPT_DIR}/config/crd/gentianos.io_appcatalogues.yaml"
    success "AppCatalogue CRD applied."

    if [[ -w /usr/local/bin ]] || sudo -n true 2>/dev/null; then
        sudo install -m 755 "${SCRIPT_DIR}/scripts/kubectl-gentian" /usr/local/bin/kubectl-gentian
        success "kubectl-gentian installed to /usr/local/bin."
    else
        warn "Cannot write to /usr/local/bin — skip. Install manually:"
        warn "  sudo install -m 755 ${SCRIPT_DIR}/scripts/kubectl-gentian /usr/local/bin/kubectl-gentian"
    fi
}

# =============================================================================
# Summary
# =============================================================================
print_summary() {
    local argocd_pw
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || echo "(not-ready)")

    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  Gentian OS bootstrap complete!                          ║${NC}"
    echo -e "${GREEN}╠══════════════════════════════════════════════════════════╣${NC}"
    echo -e "${GREEN}║  ArgoCD URL   : https://${NODE_IP}:30443${NC}"
    echo -e "${GREEN}║  ArgoCD login : admin / ${argocd_pw}${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo "  Monitor sync:   kubectl get applications -n argocd"
    echo "  Install apps:   kubectl gentian install <profile>"
}

# =============================================================================
# Main
# =============================================================================
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     Gentian OS — Fresh Cluster Bootstrap                 ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""

    prompt_credentials
    check_prereqs
    install_tools
    create_namespaces
    install_eso
    install_argocd
    setup_argocd_repos
    bootstrap_transit_app
    init_openbao_transit
    bootstrap_argocd_apps
    init_openbao
    run_tofu_openbao_init
    seed_secrets
    bootstrap_root_appset
    install_app_catalogue
    print_summary
}

main "$@"
