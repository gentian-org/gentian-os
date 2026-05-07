#!/usr/bin/env bash
# =============================================================================
# getting-started.sh — Gentian OS quick-start guide (Crossplane-based install)
# =============================================================================
# Run this before install-cp.sh to verify your environment is ready and to
# understand what the installer will do.
#
# Usage:
#   ./getting-started.sh
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
ok()   { echo -e "  ${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "  ${YELLOW}[WARN]${NC}  $*"; }
fail() { echo -e "  ${RED}[FAIL]${NC}  $*"; }

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║     Gentian OS — Getting Started                         ║${NC}"
echo -e "${CYAN}║     Crossplane-based bootstrap (Phase 1)                 ║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "  This guide helps you run: ./install-cp.sh"
echo "  The installer provisions Gentian OS kernel infrastructure using Crossplane."
echo ""

# =============================================================================
# 1. Required CLI tools
# =============================================================================
echo "━━━ Required CLI tools ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
TOOLS_MISSING=0

check_tool() {
    local cmd="$1" hint="${2:-}"
    if command -v "$cmd" &>/dev/null; then
        ok "${cmd}  ($(command -v "$cmd"))"
    else
        fail "${cmd} not found${hint:+ — $hint}"
        TOOLS_MISSING=$((TOOLS_MISSING + 1))
    fi
}

check_tool kubectl  "https://kubernetes.io/docs/tasks/tools/"
check_tool helm     "https://helm.sh/docs/intro/install/"
check_tool jq       "sudo apt install jq  /  brew install jq"
check_tool openssl  "usually pre-installed"
check_tool curl     "usually pre-installed"
check_tool bao      "https://github.com/openbao/openbao/releases"
check_tool crossplane "make install-tools  (in this repo)"
check_tool python3  "https://python.org/downloads/"

echo ""

# =============================================================================
# 2. Kubernetes cluster
# =============================================================================
echo "━━━ Kubernetes cluster ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
CLUSTER_OK=0
if kubectl cluster-info --request-timeout=5s >/dev/null 2>&1; then
    CTX=$(kubectl config current-context)
    ok "cluster reachable — context: ${CTX}"
    CLUSTER_OK=1
else
    fail "no reachable cluster — set KUBECONFIG or connect to your cluster"
fi
echo ""

# =============================================================================
# 3. Required environment variables
# =============================================================================
echo "━━━ Required secrets / config ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  install-cp.sh will prompt for any missing values, or you can pre-export"
echo "  them in install.secrets.env (secrets) and install.env (config)."
echo ""
VARS_MISSING=0

check_var() {
    local var="$1" desc="$2"
    if [[ -n "${!var:-}" ]]; then
        ok "${var}"
    else
        warn "${var} — not set (${desc})"
        VARS_MISSING=$((VARS_MISSING + 1))
    fi
}

check_var MASTER_PASSWORD              "HMAC master secret — derives all app passwords"
check_var OD_PRIVATE_REGISTRY_USERNAME "registry.opencode.de username"
check_var OD_PRIVATE_REGISTRY_PASSWORD "registry.opencode.de token/password"
check_var OD_SMTP_RELAY_USERNAME       "SMTP relay username (e.g. Gmail address)"
check_var OD_SMTP_RELAY_PASSWORD       "SMTP relay password (e.g. Gmail App Password)"
check_var KERNEL_DOMAIN                "platform-wide DNS suffix (e.g. platform.example.com)"

echo ""
echo "  Optional:"
check_var LETSENCRYPT_EMAIL   "Let's Encrypt ACME contact (defaults to admin@KERNEL_DOMAIN)"
check_var CF_API_TOKEN        "Cloudflare token for DNS-01 wildcard certs (optional)"
check_var NETWORK_MODE        "tunnel (default) or static-ip"
check_var EXTERNAL_SMTP_HOST  "required if MAIL_SERVICE_MODE=external"

echo ""

# =============================================================================
# 4. Config files
# =============================================================================
echo "━━━ Config files (optional) ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
for f in install.env install.secrets.env; do
    fp="${SCRIPT_DIR}/${f}"
    if [[ -r "${fp}" ]]; then
        ok "${f}  (will be loaded automatically)"
    else
        warn "${f}  — not found (copy from ${f}.template to use)"
    fi
done
echo ""

# =============================================================================
# 5. What the installer does
# =============================================================================
echo "━━━ What install-cp.sh does ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
printf "  %-4s  %-13s  %s\n" "Step" "Component" "Description"
printf "  %-4s  %-13s  %s\n" "────" "─────────" "───────────────────────────────────────────────────────"
printf "  %-4s  %-13s  %s\n" "0"   "Crossplane"    "Install Crossplane core (controller + RBAC)"
printf "  %-4s  %-13s  %s\n" "0b"  "Crossplane"    "Install providers: provider-kubernetes, provider-vault,"
printf "  %-4s  %-13s  %s\n" ""    ""               "  function-go-templating"
printf "  %-4s  %-13s  %s\n" "0c"  "Crossplane"    "Apply XRD (XCluster / Cluster) + Composition"
printf "  %-4s  %-13s  %s\n" "1"   "Namespaces"    "Create kernel namespaces"
printf "  %-4s  %-13s  %s\n" "2"   "Cluster"       "Pre-warm cluster (PLEG/CRI race mitigation)"
printf "  %-4s  %-13s  %s\n" "3"   "cert-manager"  "Install cert-manager via Helm"
printf "  %-4s  %-13s  %s\n" "3b"  "cert-manager"  "Apply ClusterIssuers (HTTP-01; DNS-01 if Cloudflare token)"
printf "  %-4s  %-13s  %s\n" "4"   "ESO"           "Install External Secrets Operator"
printf "  %-4s  %-13s  %s\n" "5"   "ArgoCD"        "Install ArgoCD + configure repos + Image Updater"
printf "  %-4s  %-13s  %s\n" "6"   "OpenBao"       "Bootstrap transit seal instance (ArgoCD app)"
printf "  %-4s  %-13s  %s\n" "7"   "OpenBao"       "Init transit OpenBao (auto-unseal key setup)"
printf "  %-4s  %-13s  %s\n" "8"   "OpenBao"       "Bootstrap remaining ArgoCD kernel apps"
printf "  %-4s  %-13s  %s\n" ""    ""               "  (openbao, reloader, cnpg, cnpg-cluster, globals)"
printf "  %-4s  %-13s  %s\n" "9"   "OpenBao"       "Init primary OpenBao (KV engine, recovery keys)"
printf "  %-4s  %-13s  %s\n" "10"  "OpenBao"       "Bootstrap Kubernetes auth for Crossplane"
printf "  %-4s  %-13s  %s\n" ""    ""               "  Replaces: 'tofu apply openbao-init'"
printf "  %-4s  %-13s  %s\n" "11"  "Crossplane"    "Create derived-credential K8s Secrets in crossplane-system"
printf "  %-4s  %-13s  %s\n" "12"  "Cluster XR"    "Apply Cluster claim → kernel structural resources:"
printf "  %-4s  %-13s  %s\n" ""    ""               "  • OpenBao KV mount + policies + K8s auth backend/roles"
printf "  %-4s  %-13s  %s\n" ""    ""               "  • KV seed paths (database, cache, storage, identity, mail)"
printf "  %-4s  %-13s  %s\n" ""    ""               "  • ArgoCD AppProject (gentianos-tenants)"
printf "  %-4s  %-13s  %s\n" ""    ""               "  • ESO ClusterSecretStore (openbao)"
printf "  %-4s  %-13s  %s\n" ""    ""               "  • cert-manager ClusterIssuer (letsencrypt-http01)"
printf "  %-4s  %-13s  %s\n" "12b" "Secrets"       "Seed remaining secrets: registry, DNS/Cloudflare, internal"
printf "  %-4s  %-13s  %s\n" "13"  "TLS"           "Install kernel wildcard Certificate (optional)"
echo ""
echo "  Not done in Phase 1 (coming in later phases):"
printf "  %-4s  %-13s  %s\n" "P2"  "Apps"    "Pattern B charts (Nubus, OX App Suite) via provider-helm"
printf "  %-4s  %-13s  %s\n" "P3"  "Tenants" "Tenant XRD + provisioning via Cluster XR"
echo ""

# =============================================================================
# 6. Cluster XR config
# =============================================================================
echo "━━━ Cluster XR configuration ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  The Cluster claim is at: crossplane/claims/dev-cluster.yaml"
echo "  Edit this file to set kernelDomain, ldapBaseDn, OpenBao server, etc."
echo "  before running install-cp.sh."
echo ""
CLAIM="${SCRIPT_DIR}/crossplane/claims/dev-cluster.yaml"
if [[ -r "${CLAIM}" ]]; then
    ok "dev-cluster.yaml found"
    DOMAIN=$(grep 'kernelDomain:' "${CLAIM}" | awk '{print $2}' | tr -d '"' || true)
    [[ -n "${DOMAIN}" ]] && echo "    kernelDomain: ${DOMAIN}"
else
    fail "crossplane/claims/dev-cluster.yaml not found"
fi
echo ""

# =============================================================================
# 7. Summary + run instructions
# =============================================================================
echo "━━━ Run instructions ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if [[ "${TOOLS_MISSING}" -gt 0 || "${CLUSTER_OK}" -eq 0 ]]; then
    echo -e "  ${RED}Fix the issues above, then run:${NC}"
    echo ""
else
    echo "  Everything looks good. Run:"
    echo ""
fi

echo "    ./install-cp.sh              # full bootstrap"
echo "    ./install-cp.sh --validate   # validate config only, no cluster changes"
echo ""
echo "  To undo everything:"
echo ""
echo "    ./uninstall-cp.sh            # safe teardown (preserves PVC data)"
echo "    ./uninstall-cp.sh -f         # force teardown (deletes all data)"
echo ""
echo "  Migration plan: docs/crossplane-migration-plan.md"
echo ""

if [[ "${TOOLS_MISSING}" -gt 0 || "${CLUSTER_OK}" -eq 0 ]]; then
    exit 1
fi
exit 0
