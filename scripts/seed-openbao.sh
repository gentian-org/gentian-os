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
#   ./scripts/seed-openbao.sh <master-password> [registry-user] [registry-password]
#
# Typical local run (port-forward first):
#   kubectl port-forward svc/openbao 8200:8200 -n openbao &
#   export BAO_ADDR=http://localhost:8200
#   export BAO_TOKEN="$(cat /path/to/root-token)"
#   ./scripts/seed-openbao.sh "your-master-password" "oci-user" "oci-pass"
#
# Secret paths written (under secret/gentian-os/kernel/):
#   database/postgresql
#   database/mariadb
#   cache/redis
#   storage/minio
#   identity/nubus
#   identity/keycloak-bootstrap
#   identity/intercom
#   apps/nextcloud
#   mail/postfix                  (requires args 4+5: smtp relay user/pass)
#   mail/dovecot
#   storage/registry              (optional, requires args 2+3)
# =============================================================================

MASTER_PASSWORD="${1:-sovereign-workplace}"
REGISTRY_USER="${2:-}"
REGISTRY_PASSWORD="${3:-}"
SMTP_RELAY_USER="${4:-}"
SMTP_RELAY_PASS="${5:-}"

BAO_ADDR="${BAO_ADDR:-http://localhost:8200}"
BAO_TOKEN="${BAO_TOKEN:-}"
MAIL_SERVICE_MODE="${MAIL_SERVICE_MODE:-external}"
EXTERNAL_SMTP_HOST="${EXTERNAL_SMTP_HOST:-}"
EXTERNAL_SMTP_PORT="${EXTERNAL_SMTP_PORT:-587}"
EXTERNAL_SMTP_SSL="${EXTERNAL_SMTP_SSL:-false}"
EXTERNAL_SMTP_STARTTLS="${EXTERNAL_SMTP_STARTTLS:-true}"

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

echo "=========================================="
echo "Seeding OpenBao Secrets"
echo "  OpenBao:     ${BAO_ADDR}"
echo "  Path prefix: secret/gentian-os/kernel/"
echo "=========================================="

# =============================================================================
# Password derivation
# Uses HMAC-SHA256 hex output directly. The previous implementation piped
# through sha1sum, which weakened the construction: an attacker with one known
# derived credential could run an offline dictionary attack against the master
# password at SHA-1 speed. The corrected version uses the 64-char HMAC-SHA256
# hex output directly. NOTE: this change is backward-incompatible — all derived
# passwords change. It must be applied together with a full cluster re-seed.
# =============================================================================
derive_password() {
    local context="$1"
    local purpose="$2"
    echo -n "${context}:${purpose}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" | awk '{print $2}'
}

derive_nats_password() {
    local context="$1"
    local purpose="$2"
    echo "n$(derive_password "$context" "$purpose")"
}

echo ""
echo "Deriving passwords..."

# --- PostgreSQL ---
PG_POSTGRES_PW=$(derive_password "postgres" "postgres_user")
PG_KEYCLOAK_PW=$(derive_password "postgres" "keycloak_user")
PG_KC_EXT_PW=$(derive_password "postgres" "keycloak_extensions_user")
PG_SELFSERVICE_PW=$(derive_password "postgres" "selfservice_user")
PG_AUTHSESSION_PW=$(derive_password "postgres" "authsession_user")
PG_GUARDIAN_PW=$(derive_password "postgres" "guardianmanagementapi_user")
PG_NOTIFICATIONS_PW=$(derive_password "postgres" "notificationsapi_user")
# --- MariaDB ---
MARIA_ROOT_PW=$(derive_password "mariadb" "root_password")
MARIA_OX_PW=$(derive_password "mariadb" "openxchange_user")

# --- Redis ---
REDIS_PW=$(derive_password "redis" "password")

# --- MinIO ---
MINIO_ROOT_PW=$(derive_password "minio" "root_password")
MINIO_UMS_PW=$(derive_password "minio" "ums_user")
MINIO_MIGRATIONS_PW=$(derive_password "minio" "migrations_user")
MINIO_DOVECOT_PW=$(derive_password "minio" "dovecot_user")

# --- Keycloak ---
KC_ADMIN_PW=$(derive_password "keycloak" "adminPassword")
KC_CLIENT_INTERCOM=$(derive_password "keycloak" "intercom_client_secret")

# --- LDAP ---
LDAP_ADMIN_PW=$(derive_password "cn=admin" "ldap")

# --- Nubus system ---
ADMIN_PW=$(derive_password "nubus" "Administrator")


# --- NATS ---
NATS_API_PW=$(derive_nats_password "api" "nats")
NATS_DISPATCHER_PW=$(derive_nats_password "dispatcher" "nats")
NATS_PREFILL_PW=$(derive_nats_password "prefill" "nats")
NATS_UDM_LISTENER_PW=$(derive_nats_password "udmListener" "nats")
NATS_UDM_TRANSFORMER_PW=$(derive_nats_password "udmTransformer" "nats")

# --- Dovecot ---
DOVECOT_DOVEADM_PW=$(derive_password "dovecot" "doveadm_password")

# --- SMTP (placeholder — configure real SMTP credentials manually) ---
SMTP_PW=$(derive_password "smtp" "password")

# --- Intercom ---
ICS_SESSION_SECRET=$(derive_password "intercom" "secret")
ICS_SYNAPSE_AS_TOKEN=$(derive_password "intercom" "as_token")
ICS_PORTAL_SHARED_SECRET=$(derive_password "centralnavigation" "api_key")
PORTAL_SHARED_SECRET=$(derive_password "centralnavigation" "api_key")

# --- LDAP search users (kernel services only; per-app users created by app init Jobs) ---
LDAP_SEARCH_KEYCLOAK=$(derive_password "nubus" "ldapsearch_keycloak")
LDAP_SEARCH_DOVECOT=$(derive_password "nubus" "ldapsearch_dovecot")
LDAP_SEARCH_POSTFIX=$(derive_password "nubus" "ldapsearch_postfix")

# --- Provisioning consumer API passwords (stable — injected via set_sensitive) ---
# These are passed to the portal-consumer and selfservice-consumer sub-charts so
# that Helm never auto-rotates them on upgrade, keeping NATS JetStream subscriptions valid.
PORTAL_CONSUMER_API_PW=$(derive_password "portal-consumer" "provisioning-api")
SELFSERVICE_CONSUMER_API_PW=$(derive_password "selfservice-consumer" "provisioning-api")

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
    result=$(curl -s -w "\n%{http_code}" \
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
    existing=$(curl -sf \
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
kv_put_once "internal/master-password" "$(jq -n --arg v "${MASTER_PASSWORD}" '{value: $v}')"

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

# --- Bitnami PostgreSQL (Nubus components) ----------------------------------
kv_put_once "database/postgresql" "$(cat <<EOF
{
  "postgres_password":              "${PG_POSTGRES_PW}",
  "keycloak_user_password":         "${PG_KEYCLOAK_PW}",
  "keycloak_extensions_user_password": "${PG_KC_EXT_PW}",
  "selfservice_user_password":      "${PG_SELFSERVICE_PW}",
  "authsession_user_password":      "${PG_AUTHSESSION_PW}",
  "guardianmanagementapi_user_password": "${PG_GUARDIAN_PW}",
  "notificationsapi_user_password": "${PG_NOTIFICATIONS_PW}"
}
EOF
)"

# --- MariaDB ---
kv_put_once "database/mariadb" "$(cat <<EOF
{
  "root_password":        "${MARIA_ROOT_PW}",
  "openxchange_password": "${MARIA_OX_PW}"
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
  "root_user":             "minio",
  "root_password":         "${MINIO_ROOT_PW}",
  "ums_password":          "${MINIO_UMS_PW}",
  "migrations_password":   "${MINIO_MIGRATIONS_PW}",
  "dovecot_password":      "${MINIO_DOVECOT_PW}"
}
EOF
)"

# --- Nubus ---
kv_put_once "identity/nubus" "$(cat <<EOF
{
  "master_password":            "${MASTER_PASSWORD}",
  "admin_password":             "${ADMIN_PW}",
  "ldap_admin_password":        "${LDAP_ADMIN_PW}",
  "keycloak_admin_password":    "${KC_ADMIN_PW}",

  "smtp_password":              "${SMTP_PW}",
  "nats_api_password":          "${NATS_API_PW}",
  "nats_dispatcher_password":   "${NATS_DISPATCHER_PW}",
  "nats_prefill_password":      "${NATS_PREFILL_PW}",
  "nats_udm_listener_password": "${NATS_UDM_LISTENER_PW}",
  "nats_udm_transformer_password": "${NATS_UDM_TRANSFORMER_PW}",
  "minio_ums_secret_access_key": "${MINIO_UMS_PW}",
  "pg_selfservice_password":    "${PG_SELFSERVICE_PW}",
  "pg_authsession_password":    "${PG_AUTHSESSION_PW}",
  "pg_keycloak_password":       "${PG_KEYCLOAK_PW}",
  "pg_keycloak_extensions_password": "${PG_KC_EXT_PW}",
  "pg_guardian_password":       "${PG_GUARDIAN_PW}",
  "pg_notifications_password":  "${PG_NOTIFICATIONS_PW}",
  "ldapsearch_keycloak":        "${LDAP_SEARCH_KEYCLOAK}",
  "ldapsearch_dovecot":         "${LDAP_SEARCH_DOVECOT}",
  "ldapsearch_postfix":         "${LDAP_SEARCH_POSTFIX}",
  "portal_shared_secret":             "${PORTAL_SHARED_SECRET}",
  "portal_consumer_api_password":     "${PORTAL_CONSUMER_API_PW}",
  "selfservice_consumer_api_password": "${SELFSERVICE_CONSUMER_API_PW}"
}
EOF
)"
# NOTE: If gentian-os/kernel/identity/nubus already exists (existing cluster), the above
# kv_put_once was skipped. Add the two new consumer password keys manually:
#   bao kv patch secret/gentian-os/kernel/identity/nubus \
#     portal_consumer_api_password=<derive_password output> \
#     selfservice_consumer_api_password=<derive_password output>
# Then force-reconcile the affected Terraform CR.

# --- Collabora (kernel office service) ---
COLLABORA_ADMIN_PW=$(derive_password "collabora" "admin_password")
kv_put_once "apps/collabora" "$(cat <<EOF
{
  "admin_password": "${COLLABORA_ADMIN_PW}"
}
EOF
)"

# --- Nextcloud ---
NC_ADMIN_PW=$(derive_password "nextcloud" "admin_password")
NC_STATUS_PW=$(derive_password "nextcloud" "status_password")
NC_OIDC_SECRET=$(derive_password "nextcloud" "oidc_client_secret")
NC_INTEGRATION_PW=$(derive_password "nextcloud" "integration_password")
NC_METRICS_TOKEN=$(derive_password "nextcloud" "metrics_token")
NC_MINIO_PW=$(derive_password "nextcloud" "minio_password")
# Derivation contexts kept identical to original so existing installs stay compatible
NC_LDAP_PW=$(derive_password "nubus" "ldapsearch_nextcloud")
NC_DB_PW=$(derive_password "postgres" "nextcloud_user")
kv_put_once "apps/nextcloud" "$(cat <<EOF
{
  "admin_password":       "${NC_ADMIN_PW}",
  "status_password":      "${NC_STATUS_PW}",
  "oidc_client_secret":   "${NC_OIDC_SECRET}",
  "integration_password": "${NC_INTEGRATION_PW}",
  "metrics_token":        "${NC_METRICS_TOKEN}",
  "minio_password":       "${NC_MINIO_PW}",
  "ldapsearch_password":  "${NC_LDAP_PW}",
  "db_password":          "${NC_DB_PW}"
}
EOF
)"

# --- Intercom ---
kv_put_once "identity/intercom" "$(cat <<EOF
{
  "session_secret":               "${ICS_SESSION_SECRET}",
  "oidc_client_secret":           "${KC_CLIENT_INTERCOM}",
  "matrix_as_token":              "${ICS_SYNAPSE_AS_TOKEN}",
  "portal_shared_secret":         "${ICS_PORTAL_SHARED_SECRET}",
  "redis_auth_password":          "${REDIS_PW}"
}
EOF
)"

# --- Keycloak Bootstrap ---
kv_put_once "identity/keycloak-bootstrap" "$(cat <<EOF
{
  "admin_password":          "${KC_ADMIN_PW}",
  "intercom_client_secret":  "${KC_CLIENT_INTERCOM}"
}
EOF
)"

# --- Dovecot ---
# doveadm_password: HMAC-derived for reproducibility
# oidc_client_secret: written by the keycloak-config Crossplane composition on first run.
#   If the keycloak-config workspace has already run, this kv_put_once is a no-op because
#   the secret already exists with oidc_client_secret.  In that case, patch manually:
#     bao kv patch gentian-os/kernel/mail/dovecot doveadm_password=<value>
_DOVECOT_EXISTING=$(curl -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
  "${BAO_ADDR}/v1/secret/data/gentian-os/kernel/mail/dovecot" 2>/dev/null || true)
if echo "${_DOVECOT_EXISTING}" | grep -q '"doveadm_password"'; then
  echo "  Skipping gentian-os/kernel/mail/dovecot (doveadm_password already exists)"
elif echo "${_DOVECOT_EXISTING}" | grep -q '"data":{'; then
  # Secret exists (created by keycloak-config) but missing doveadm_password — patch it
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

# --- Mail transport settings consumed by Nubus rendering ---
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
# Optional: only required when KERNEL_DOMAIN is served via Cloudflare and the
# kernel wildcard Certificate is enabled (see docs/design/multi-tenancy.md §3).
# Sourced from CF_API_TOKEN env var to keep the positional-arg contract stable.
if [ -n "${CF_API_TOKEN:-}" ]; then
    kv_put_once "dns/cloudflare" "$(jq -n \
        --arg api_token "${CF_API_TOKEN}" \
        '{"api-token": $api_token}')"
else
    echo ""
    echo "  Skipping Cloudflare API token (CF_API_TOKEN not set)."
    echo "  To add it later: bao kv put gentian-os/kernel/dns/cloudflare api-token=<token>"
fi

echo ""
echo "=========================================="
echo "✅ All secrets seeded into OpenBao!"
echo ""
echo "Verify with:"
echo "  bao kv list gentian-os/kernel"
echo "  bao kv get secret/gentian-os/kernel/database/postgresql"
echo "=========================================="
