#!/bin/bash
set -euo pipefail

# =============================================================================
# Seed Initial Secrets into OpenBao
# =============================================================================
#
# Derives all application passwords using the same HMAC-SHA256 logic as the
# legacy generate-passwords.sh, then writes them directly to OpenBao via the
# HTTP API.  No plaintext passwords are stored in Git or Terraform state.
#
# Usage:
#   export BAO_ADDR=http://localhost:8200     # port-forwarded OpenBao
#   export BAO_TOKEN="<root-or-admin-token>"
#   ./scripts/bootstrap/seed-openbao.sh <master-password> [smtp-relay-user] [smtp-relay-password]
#
# Typical local run (port-forward first):
#   kubectl port-forward svc/openbao 8200:8200 -n openbao &
#   export BAO_ADDR=http://localhost:8200
#   export BAO_TOKEN="$(cat /path/to/root-token)"
#   ./scripts/bootstrap/seed-openbao.sh "your-master-password" "oci-user" "oci-pass"
#
# Secret paths written (under secret/gentian-os/kernel/):
#   database/postgresql
#   database/mariadb
#   cache/redis
#   storage/minio
#   identity/keycloak-bootstrap
#   authz/openfga
#   mail/postfix                  (requires args 4+5: smtp relay user/pass)
#   mail/dovecot
#   storage/registry              (optional, requires args 2+3)
# =============================================================================

MASTER_PASSWORD="${1:-}"
SMTP_RELAY_USER="${2:-}"
SMTP_RELAY_PASS="${3:-}"
REGISTRY_USER="${REGISTRY_USER:-}"
REGISTRY_PASSWORD="${REGISTRY_PASSWORD:-}"

BAO_ADDR="${BAO_ADDR:-http://localhost:8200}"
BAO_TOKEN="${BAO_TOKEN:-}"
MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
EXTERNAL_SMTP_HOST="${EXTERNAL_SMTP_HOST:-}"
EXTERNAL_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}"
EXTERNAL_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}"
EXTERNAL_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}"
SECRET_MODE="${SECRET_MODE:-derived}"
export VAULT_SKIP_VERIFY=true

if [ -z "$BAO_TOKEN" ]; then
    echo "Error: BAO_TOKEN environment variable is not set."
    echo "Set it to a root or admin token before running this script."
    exit 1
fi

# Check prerequisites
for cmd in openssl sha1sum curl jq; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "Error: $cmd is not installed."
        exit 1
    fi
done

# Try to read existing master-password and salt from OpenBao
existing_secret=$(curl -k -sf -H "X-Vault-Token: ${BAO_TOKEN}" "${BAO_ADDR}/v1/secret/data/gentian-os/kernel/internal/master-password" 2>/dev/null || true)
existing_master=$(echo "${existing_secret}" | jq -r '.data.data.value // empty' 2>/dev/null || true)
existing_salt=$(echo "${existing_secret}" | jq -r '.data.data.salt // empty' 2>/dev/null || true)

if [ -n "${existing_master}" ]; then
    MASTER_PASSWORD="${existing_master}"
fi

if [ -z "${MASTER_PASSWORD}" ]; then
    echo "Error: MASTER_PASSWORD is empty. A master password must be provided as the first argument or pre-seeded in OpenBao." >&2
    exit 1
fi

validate_master_password_entropy() {
    local mp="$1"
    if [ ${#mp} -lt 16 ]; then
        echo "Error: MASTER_PASSWORD is too weak. It must be at least 16 characters long." >&2
        exit 1
    fi
}
validate_master_password_entropy "${MASTER_PASSWORD}"

if [ -n "${existing_salt}" ]; then
    DERIVATION_SALT="${existing_salt}"
elif [ -n "${existing_master}" ]; then
    DERIVATION_SALT=""
else
    DERIVATION_SALT="${DERIVATION_SALT:-$(openssl rand -hex 16)}"
fi
export DERIVATION_SALT

echo "=========================================="
echo "Seeding OpenBao Secrets"
echo "  OpenBao:     ${BAO_ADDR}"
echo "  Path prefix: secret/gentian-os/kernel/"
echo "  Mode:        ${SECRET_MODE}"
echo "=========================================="

# =============================================================================
# Password derivation
# Uses HMAC-SHA256 hex output directly.
# Updated to support per-cluster random salting and SECRET_MODE=random.
# =============================================================================
derive_password() {
    local context="$1"
    local purpose="$2"
    if [ "${SECRET_MODE:-derived}" = "random" ]; then
        openssl rand -hex 32
    else
        echo -n "${context}:${purpose}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" | awk '{print $2}'
    fi
}


echo ""
echo "Deriving passwords..."

# --- PostgreSQL ---
PG_POSTGRES_PW=$(derive_password "postgres" "postgres_user")
PG_KEYCLOAK_PW=$(derive_password "postgres" "keycloak_user")
PG_KC_EXT_PW=$(derive_password "postgres" "keycloak_extensions_user")
PG_OPENFGA_PW=$(derive_password "postgres" "openfga_user")
# --- MariaDB ---
MARIA_ROOT_PW=$(derive_password "mariadb" "root_password")

# --- Redis ---
REDIS_PW=$(derive_password "redis" "password")

# --- MinIO ---
MINIO_ROOT_PW=$(derive_password "minio" "root_password")

# --- Keycloak ---
KC_ADMIN_PW=$(derive_password "keycloak" "adminPassword")

# --- Dovecot ---
DOVECOT_DOVEADM_PW=$(derive_password "dovecot" "doveadm_password")


echo "  All passwords derived."

# =============================================================================
# Write to OpenBao via HTTP API (KV v2)
# POST /v1/secret/data/gentian-os/kernel/<category>/<component>
# Body: {"data": {"key": "value", ...}}
# =============================================================================

kv_put() {
    local path="$1"
    local json_data="$2"
    local full_path="secret/data/gentian-os/kernel/${path}"
    echo "  Writing ${full_path}..."
    local result http_code body
    result=$(curl -k -s -w "\n%{http_code}" \
        -H "X-Vault-Token: ${BAO_TOKEN}" \
        -H "Content-Type: application/json" \
        -X POST \
        -d "{\"data\": ${json_data}}" \
        "${BAO_ADDR}/v1/${full_path}")
    http_code=$(echo "$result" | tail -1)
    body=$(echo "$result" | head -n -1)
    if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
        echo "    ERROR (HTTP $http_code): $body" >&2
        return 1
    fi
    if echo "$body" | grep -q '"errors"'; then
        echo "    ERROR: $body" >&2
        return 1
    fi
    echo "    OK (version: $(echo "$body" | grep -o '"version":[0-9]*' | head -1))"
}

# kv_put_once writes a secret only if the path does not already exist.
# Use this for all HMAC-derived credentials so that re-running seed-openbao.sh
# with a different MASTER_PASSWORD cannot overwrite live credentials and create
# drift between Vault and an already-deployed cluster.
# To intentionally rotate: use 'bao kv patch' or 'bao kv put' directly, then
# force-reconcile the affected ArgoCD Application or Terraform CR.
kv_put_once() {
    local path="$1"
    local json_data="$2"
    local check_path="secret/data/gentian-os/kernel/${path}"
    local existing
    existing=$(curl -k -sf \
        -H "X-Vault-Token: ${BAO_TOKEN}" \
        "${BAO_ADDR}/v1/${check_path}" 2>/dev/null || true)
    if echo "${existing}" | grep -q '"data":{'; then
        echo "  Skipping gentian-os/kernel/${path} (already exists — use 'bao kv patch' to update)"
        return 0
    fi
    kv_put "${path}" "${json_data}"
}

echo ""
echo "Writing secrets to OpenBao..."

# --- Master password (consumed by gentian-os operator at startup) -----------
# The operator reads MASTER_PASSWORD from this canonical path and feeds it
# into its HKDF-SHA256 deriver to produce per-tenant per-app credentials
# deterministically. Path matches secrets.MasterPasswordPath in
# internal/kernel/secrets/paths.go.
kv_put_once "internal/master-password" "$(jq -n --arg v "${MASTER_PASSWORD}" --arg s "${DERIVATION_SALT}" '{value: $v, salt: $s}')"

# --- PostgreSQL ---
# --- CNPG superuser (shared CloudNativePG Cluster in platform-kernel) ------
CNPG_SUPERUSER_PW=$(derive_password "cnpg" "superuser")

kv_put_once "database/cnpg" "$(cat <<EOF
{
  "superuser_username": "postgres",
  "superuser_password": "${CNPG_SUPERUSER_PW}"
}
EOF
)"

# --- Bitnami PostgreSQL (kernel platform services) ---------------------------
kv_put_once "database/postgresql" "$(cat <<EOF
{
  "postgres_password":              "${PG_POSTGRES_PW}",
  "keycloak_user_password":         "${PG_KEYCLOAK_PW}",
  "keycloak_extensions_user_password": "${PG_KC_EXT_PW}",
  "openfga_user_password":           "${PG_OPENFGA_PW}"
}
EOF
)"

# --- OpenFGA (Stage 1 ReBAC PDP) --------------------------------------------
OPENFGA_PRESHARED=$(derive_password "openfga" "preshared_key")
kv_put_once "authz/openfga" "$(cat <<EOF
{
  "preshared_key": "${OPENFGA_PRESHARED}"
}
EOF
)"

# --- MariaDB ---
kv_put_once "database/mariadb" "$(cat <<EOF
{
  "root_password": "${MARIA_ROOT_PW}"
}
EOF
)"

# --- Redis ---
kv_put_once "cache/redis" "$(cat <<EOF
{
  "auth_password": "${REDIS_PW}"
}
EOF
)"

# --- MinIO ---
kv_put_once "storage/minio" "$(cat <<EOF
{
  "root_user":     "minio",
  "root_password": "${MINIO_ROOT_PW}"
}
EOF
)"

# --- Keycloak Bootstrap (Suze IdP admin) ---
kv_put_once "identity/keycloak-bootstrap" "$(cat <<EOF
{
  "admin_password": "${KC_ADMIN_PW}"
}
EOF
)"

# --- Dovecot ---
# doveadm_password: HMAC-derived for reproducibility
# oidc_client_secret: generated on first seed when absent.
_DOVECOT_EXISTING=$(curl -k -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
  "${BAO_ADDR}/v1/secret/data/gentian-os/kernel/mail/dovecot" 2>/dev/null || true)
if echo "${_DOVECOT_EXISTING}" | grep -q '"doveadm_password"'; then
  echo "  Skipping gentian-os/kernel/mail/dovecot (doveadm_password already exists)"
elif echo "${_DOVECOT_EXISTING}" | grep -q '"data":{'; then
  # Secret exists but missing doveadm_password — patch it
  echo "  Patching gentian-os/kernel/mail/dovecot (adding doveadm_password)..."
  EXISTING_OIDC=$(echo "${_DOVECOT_EXISTING}" | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['data']['data']['oidc_client_secret'])" 2>/dev/null || echo "")
  if [ -n "${EXISTING_OIDC}" ]; then
    kv_put "mail/dovecot" "$(cat <<EOF
{
  "doveadm_password":   "${DOVECOT_DOVEADM_PW}",
  "oidc_client_secret": "${EXISTING_OIDC}"
}
EOF
)"
  else
    echo "    ERROR: Could not read existing oidc_client_secret. Patch manually."
  fi
else
  # Secret does not exist at all — write both keys
  DOVECOT_OIDC_SECRET=$(openssl rand -hex 20)
  kv_put "mail/dovecot" "$(cat <<EOF
{
  "doveadm_password":   "${DOVECOT_DOVEADM_PW}",
  "oidc_client_secret": "${DOVECOT_OIDC_SECRET}"
}
EOF
)"
fi

# --- Postfix (SMTP relay credentials) ---
if [ -n "$SMTP_RELAY_USER" ] && [ -n "$SMTP_RELAY_PASS" ]; then
    kv_put_once "mail/postfix" "$(jq -n \
        --arg relay_username "${SMTP_RELAY_USER}" \
        --arg relay_password "${SMTP_RELAY_PASS}" \
        '{"relay_username": $relay_username, "relay_password": $relay_password}')"
else
    echo ""
    echo "  Skipping postfix credentials (not provided)."
    echo "  To add them later: bao kv put gentian-os/kernel/mail/postfix relay_username=<u> relay_password=<p>"
fi

# --- Mail transport settings ---
# This path is operational config (not a derived password), so we intentionally
# overwrite it on each run to reflect install.env changes.
if [ "${MAIL_SERVICE_MODE}" != "external" ] && [ "${MAIL_SERVICE_MODE}" != "kernel" ]; then
  echo "  Invalid MAIL_SERVICE_MODE='${MAIL_SERVICE_MODE}' (expected external|kernel); defaulting to external"
  MAIL_SERVICE_MODE="external"
fi

if [ "${MAIL_SERVICE_MODE}" = "external" ] && [ -z "${EXTERNAL_SMTP_HOST}" ]; then
  echo "  MAIL_SERVICE_MODE=external but EXTERNAL_SMTP_HOST is empty; SMTP delivery will be broken."
fi

kv_put "mail/smtp" "$(jq -n \
  --arg mode "${MAIL_SERVICE_MODE}" \
  --arg host "${EXTERNAL_SMTP_HOST}" \
  --arg port "${EXTERNAL_SMTP_PORT}" \
  --arg ssl "${EXTERNAL_SMTP_SSL}" \
  --arg starttls "${EXTERNAL_SMTP_STARTTLS}" \
  --arg username "${SMTP_RELAY_USER}" \
  '{
    "mode": $mode,
    "host": $host,
    "port": $port,
    "ssl": $ssl,
    "starttls": $starttls,
    "username": $username
  }')"

# --- Registry (OCI pull credentials) — only if provided ---
if [ -n "$REGISTRY_USER" ] && [ -n "$REGISTRY_PASSWORD" ]; then
    kv_put_once "storage/registry" "$(jq -n \
        --arg username "${REGISTRY_USER}" \
        --arg password "${REGISTRY_PASSWORD}" \
        '{"username": $username, "password": $password}')"
else
    echo ""
    echo "  Skipping registry credentials (not provided)."
    echo "  To add them later: bao kv put gentian-os/kernel/storage/registry username=<u> password=<p>"
fi

# --- Cloudflare API token (DNS-01 ACME for kernel wildcard) ---
# The zone credential, under the path its provider owns in kernel/platforms.yaml.
#
# Only required when the cluster solves DNS-01 — a wildcard certificate is the
# only thing that needs it. Cloudflare carries two extra fields no other
# provider has: the zone id and tunnel CNAME the operator's optional edge-DNS
# adapter reads. They are written alongside the token rather than at a path of
# their own, because they are the same account's configuration.
DNS_PROVIDER="${DNS_PROVIDER:-cloudflare}"
if [ "${DNS_PROVIDER}" = "cloudflare" ] && [ -n "${CF_API_TOKEN:-}" ]; then
    kv_put "dns/cloudflare" "$(jq -n \
        --arg api_token "${CF_API_TOKEN}" \
        --arg zone_id "${CF_ZONE_ID:-}" \
        --arg tunnel_cname "${CF_TUNNEL_CNAME:-}" \
        '{"api-token": $api_token, "zone-id": $zone_id, "tunnel-cname": $tunnel_cname}')"
elif [ "${DNS_PROVIDER}" != "cloudflare" ] && [ "${DNS_PROVIDER}" != "none" ] && [ -n "${GENTIAN_DNS_FIELDS_JSON:-}" ]; then
    # Every other provider: the installer collected its fields by name from the
    # catalogue and handed them over as one JSON object, so this stays a single
    # write regardless of how many fields the provider has.
    kv_put "dns/${DNS_PROVIDER}" "${GENTIAN_DNS_FIELDS_JSON}"
else
    echo ""
    echo "  Skipping the DNS-01 credential for provider ${DNS_PROVIDER}."
    echo "  To add it later, supply it to the credential manager, or:"
    echo "    bao kv put gentian-os/kernel/dns/${DNS_PROVIDER} <field>=<value>"
fi

# --- LLM serving (LiteLLM / vLLM credentials) ---
KERNEL_REALM="${KERNEL_REALM:-kernel}"
LITELLM_UI_USERNAME="administrator@${KERNEL_DOMAIN}"
LITELLM_UI_PASSWORD=$(derive_password "portal-bootstrap" "administrator_password")
VLLM_API_KEY=$(derive_password "llm" "vllm_api_key")
LITELLM_MASTER_KEY=$(derive_password "llm" "litellm_master_key")
LITELLM_DB_PW=$(derive_password "llm" "litellm_db_password")
LITELLM_REDIS_PW=$(derive_password "llm" "litellm_redis_password")
# Admin console SSO (platform-admin only; see docs/design/llms.md and the
# litellm-dashboard Keycloak client provisioned by portal-login-bootstrap.sh).
LITELLM_PROXY_BASE_URL="https://llm.${KERNEL_DOMAIN}"
LITELLM_SSO_AUTH_ENDPOINT="https://id.${KERNEL_DOMAIN}/auth/realms/${KERNEL_REALM}/protocol/openid-connect/auth"
LITELLM_SSO_TOKEN_ENDPOINT="https://id.${KERNEL_DOMAIN}/auth/realms/${KERNEL_REALM}/protocol/openid-connect/token"
LITELLM_SSO_USERINFO_ENDPOINT="https://id.${KERNEL_DOMAIN}/auth/realms/${KERNEL_REALM}/protocol/openid-connect/userinfo"

kv_put "llm" "$(cat <<EOF
{
  "vllm_api_key": "${VLLM_API_KEY}",
  "litellm_master_key": "${LITELLM_MASTER_KEY}",
  "litellm_db_password": "${LITELLM_DB_PW}",
  "litellm_redis_password": "${LITELLM_REDIS_PW}",
  "litellm_ui_username": "${LITELLM_UI_USERNAME}",
  "litellm_ui_password": "${LITELLM_UI_PASSWORD}",
  "litellm_proxy_base_url": "${LITELLM_PROXY_BASE_URL}",
  "litellm_sso_authorization_endpoint": "${LITELLM_SSO_AUTH_ENDPOINT}",
  "litellm_sso_token_endpoint": "${LITELLM_SSO_TOKEN_ENDPOINT}",
  "litellm_sso_userinfo_endpoint": "${LITELLM_SSO_USERINFO_ENDPOINT}"
}
EOF
)"

echo ""
echo "=========================================="
echo "✅ All secrets seeded into OpenBao!"
echo ""
echo "Verify with:"
echo "  bao kv list gentian-os/kernel"
echo "  bao kv get secret/gentian-os/kernel/database/postgresql"
echo "=========================================="
