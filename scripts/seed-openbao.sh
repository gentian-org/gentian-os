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
#   ./scripts/seed-openbao.sh <env> <master-password> [registry-user] [registry-password]
#
# Typical local run (port-forward first):
#   kubectl port-forward svc/openbao 8200:8200 -n openbao &
#   export BAO_ADDR=http://localhost:8200
#   export BAO_TOKEN="$(cat /path/to/root-token)"
#   ./scripts/seed-openbao.sh dev "your-master-password" "oci-user" "oci-pass"
#
# Secret paths written:
#   secret/gentian/<env>/postgresql
#   secret/gentian/<env>/mariadb
#   secret/gentian/<env>/redis
#   secret/gentian/<env>/minio
#   secret/gentian/<env>/nubus
#   secret/gentian/<env>/nextcloud
#   secret/gentian/<env>/intercom
#   secret/gentian/<env>/keycloak-bootstrap
#   secret/gentian/<env>/postfix             (requires args 5+6: smtp relay user/pass)
#   secret/gentian/<env>/registry            (optional, requires args 3+4)
# =============================================================================

ENV="${1:-}"
MASTER_PASSWORD="${2:-sovereign-workplace}"
REGISTRY_USER="${3:-}"
REGISTRY_PASSWORD="${4:-}"
SMTP_RELAY_USER="${5:-}"
SMTP_RELAY_PASS="${6:-}"

if [ -z "$ENV" ]; then
    echo "Usage: $0 <env> <master-password> [registry-user] [registry-password]"
    echo "Example: $0 dev my-master-password registry-user registry-pass"
    exit 1
fi

BAO_ADDR="${BAO_ADDR:-http://localhost:8200}"
BAO_TOKEN="${BAO_TOKEN:-}"

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
echo "  Environment: ${ENV}"
echo "  Path prefix: secret/gentian/${ENV}/"
echo "=========================================="

# =============================================================================
# Password derivation (mirrors generate-passwords.sh)
# =============================================================================
derive_password() {
    local context="$1"
    local purpose="$2"
    echo -n "${context}:${purpose}" | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary | sha1sum | awk '{print $1}'
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
PG_NEXTCLOUD_PW=$(derive_password "postgres" "nextcloud_user")

# --- MariaDB ---
MARIA_ROOT_PW=$(derive_password "mariadb" "root_password")
MARIA_OX_PW=$(derive_password "mariadb" "openxchange_user")

# --- Redis ---
REDIS_PW=$(derive_password "redis" "password")

# --- MinIO ---
MINIO_ROOT_PW=$(derive_password "minio" "root_password")
MINIO_UMS_PW=$(derive_password "minio" "ums_user")
MINIO_NEXTCLOUD_PW=$(derive_password "minio" "nextcloud_user")
MINIO_OX_PW=$(derive_password "minio" "openxchange_user")
MINIO_OPENPROJECT_PW=$(derive_password "minio" "openproject_user")
MINIO_NOTES_PW=$(derive_password "minio" "notes_user")
MINIO_MIGRATIONS_PW=$(derive_password "minio" "migrations_user")
MINIO_DOVECOT_PW=$(derive_password "minio" "dovecot_user")

# --- Keycloak ---
KC_ADMIN_PW=$(derive_password "keycloak" "adminPassword")
KC_CLIENT_INTERCOM=$(derive_password "keycloak" "intercom_client_secret")

# --- LDAP ---
LDAP_ADMIN_PW=$(derive_password "cn=admin" "ldap")

# --- Nubus system ---
ADMIN_PW=$(derive_password "nubus" "Administrator")
OX_SYSTEM_PW=$(derive_password "nubus" "ox_system_user")

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

# --- LDAP search users ---
LDAP_SEARCH_KEYCLOAK=$(derive_password "nubus" "ldapsearch_keycloak")
LDAP_SEARCH_NEXTCLOUD=$(derive_password "nubus" "ldapsearch_nextcloud")
LDAP_SEARCH_DOVECOT=$(derive_password "nubus" "ldapsearch_dovecot")
LDAP_SEARCH_ELEMENT=$(derive_password "nubus" "ldapsearch_element")
LDAP_SEARCH_OX=$(derive_password "nubus" "ldapsearch_ox")
LDAP_SEARCH_POSTFIX=$(derive_password "nubus" "ldapsearch_postfix")
LDAP_SEARCH_OPENPROJECT=$(derive_password "nubus" "ldapsearch_openproject")
LDAP_SEARCH_XWIKI=$(derive_password "nubus" "ldapsearch_xwiki")

# --- Provisioning consumer API passwords (stable — injected via set_sensitive) ---
# These are passed to the portal-consumer and selfservice-consumer sub-charts so
# that Helm never auto-rotates them on upgrade, keeping NATS JetStream subscriptions valid.
PORTAL_CONSUMER_API_PW=$(derive_password "portal-consumer" "provisioning-api")
SELFSERVICE_CONSUMER_API_PW=$(derive_password "selfservice-consumer" "provisioning-api")

echo "  All passwords derived."

# =============================================================================
# Write to OpenBao via HTTP API (KV v2)
# POST /v1/secret/data/gentian/<env>/<component>
# Body: {"data": {"key": "value", ...}}
# =============================================================================

kv_put() {
    local path="$1"
    local json_data="$2"
    local full_path="secret/data/gentian/${ENV}/${path}"
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
# trigger Tofu reconciliation.
kv_put_once() {
    local path="$1"
    local json_data="$2"
    local check_path="secret/data/gentian/${ENV}/${path}"
    local existing
    existing=$(curl -sf \
        -H "X-Vault-Token: ${BAO_TOKEN}" \
        "${BAO_ADDR}/v1/${check_path}" 2>/dev/null || true)
    if echo "${existing}" | grep -q '"data":{'; then
        echo "  Skipping secret/gentian/${ENV}/${path} (already exists — use 'bao kv patch' to update)"
        return 0
    fi
    kv_put "${path}" "${json_data}"
}

echo ""
echo "Writing secrets to OpenBao..."

# --- PostgreSQL ---
kv_put_once "postgresql" "$(cat <<EOF
{
  "postgres_password":              "${PG_POSTGRES_PW}",
  "keycloak_user_password":         "${PG_KEYCLOAK_PW}",
  "keycloak_extensions_user_password": "${PG_KC_EXT_PW}",
  "selfservice_user_password":      "${PG_SELFSERVICE_PW}",
  "authsession_user_password":      "${PG_AUTHSESSION_PW}",
  "guardianmanagementapi_user_password": "${PG_GUARDIAN_PW}",
  "notificationsapi_user_password": "${PG_NOTIFICATIONS_PW}",
  "nextcloud_user_password":         "${PG_NEXTCLOUD_PW}"
}
EOF
)"

# --- MariaDB ---
kv_put_once "mariadb" "$(cat <<EOF
{
  "root_password":        "${MARIA_ROOT_PW}",
  "openxchange_password": "${MARIA_OX_PW}"
}
EOF
)"

# --- Redis ---
kv_put_once "redis" "$(cat <<EOF
{
  "auth_password": "${REDIS_PW}"
}
EOF
)"

# --- MinIO ---
kv_put_once "minio" "$(cat <<EOF
{
  "root_user":             "minio",
  "root_password":         "${MINIO_ROOT_PW}",
  "ums_password":          "${MINIO_UMS_PW}",
  "nextcloud_password":    "${MINIO_NEXTCLOUD_PW}",
  "openxchange_password":  "${MINIO_OX_PW}",
  "openproject_password":  "${MINIO_OPENPROJECT_PW}",
  "notes_password":        "${MINIO_NOTES_PW}",
  "migrations_password":   "${MINIO_MIGRATIONS_PW}",
  "dovecot_password":      "${MINIO_DOVECOT_PW}"
}
EOF
)"

# --- Nubus ---
kv_put_once "nubus" "$(cat <<EOF
{
  "master_password":            "${MASTER_PASSWORD}",
  "admin_password":             "${ADMIN_PW}",
  "ldap_admin_password":        "${LDAP_ADMIN_PW}",
  "keycloak_admin_password":    "${KC_ADMIN_PW}",
  "ox_system_user_password":    "${OX_SYSTEM_PW}",
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
  "ldapsearch_nextcloud":       "${LDAP_SEARCH_NEXTCLOUD}",
  "ldapsearch_dovecot":         "${LDAP_SEARCH_DOVECOT}",
  "ldapsearch_element":         "${LDAP_SEARCH_ELEMENT}",
  "ldapsearch_ox":              "${LDAP_SEARCH_OX}",
  "ldapsearch_postfix":         "${LDAP_SEARCH_POSTFIX}",
  "ldapsearch_openproject":     "${LDAP_SEARCH_OPENPROJECT}",
  "ldapsearch_xwiki":           "${LDAP_SEARCH_XWIKI}",
  "portal_shared_secret":             "${PORTAL_SHARED_SECRET}",
  "portal_consumer_api_password":     "${PORTAL_CONSUMER_API_PW}",
  "selfservice_consumer_api_password": "${SELFSERVICE_CONSUMER_API_PW}"
}
EOF
)"
# NOTE: If secret/gentian/${ENV}/nubus already exists (existing cluster), the above
# kv_put_once was skipped. Add the two new consumer password keys manually:
#   bao kv patch secret/gentian/${ENV}/nubus \
#     portal_consumer_api_password=<derive_password output> \
#     selfservice_consumer_api_password=<derive_password output>
# Then trigger a Tofu reconciliation.

# --- Nextcloud ---
NC_ADMIN_PW=$(derive_password "nextcloud" "admin_password")
NC_STATUS_PW=$(derive_password "nextcloud" "status_password")
NC_OIDC_SECRET=$(derive_password "nextcloud" "oidc_client_secret")
NC_INTEGRATION_PW=$(derive_password "nextcloud" "integration_password")
NC_METRICS_TOKEN=$(derive_password "nextcloud" "metrics_token")
kv_put_once "nextcloud" "$(cat <<EOF
{
  "admin_password":       "${NC_ADMIN_PW}",
  "status_password":      "${NC_STATUS_PW}",
  "oidc_client_secret":   "${NC_OIDC_SECRET}",
  "integration_password": "${NC_INTEGRATION_PW}",
  "metrics_token":        "${NC_METRICS_TOKEN}"
}
EOF
)"

# --- Intercom ---
kv_put_once "intercom" "$(cat <<EOF
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
kv_put_once "keycloak-bootstrap" "$(cat <<EOF
{
  "admin_password":          "${KC_ADMIN_PW}",
  "intercom_client_secret":  "${KC_CLIENT_INTERCOM}"
}
EOF
)"

# --- Tofu State (MinIO S3 backend — random credentials, not HMAC-derived) ---
# Uses kv_put_once like all other paths — the check above handles the skip.
_TOFU_STATE_EXISTING=$(curl -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
  "${BAO_ADDR}/v1/secret/data/gentian/${ENV}/tofu-state" 2>/dev/null || true)
if echo "${_TOFU_STATE_EXISTING}" | grep -q '"data":{'; then
  echo "  Skipping secret/gentian/${ENV}/tofu-state (already exists — not regenerated)"
else
  TOFU_STATE_ACCESS_KEY=$(openssl rand -hex 10 | tr '[:lower:]' '[:upper:]')
  TOFU_STATE_SECRET_KEY=$(openssl rand -hex 20)
  kv_put "tofu-state" "$(cat <<EOF
{
  "access_key_id":     "${TOFU_STATE_ACCESS_KEY}",
  "secret_access_key": "${TOFU_STATE_SECRET_KEY}"
}
EOF
)"
fi

# --- OX App Suite ---
OX_ADMIN_PW=$(derive_password "ox_appsuite" "admin_password")
OX_HZ_GROUP_PW=$(derive_password "ox_appsuite" "hz_group_password")
OX_BASIC_AUTH_PW=$(derive_password "ox_appsuite" "basic_auth_password")
OX_JOLOKIA_PW=$(derive_password "ox_appsuite" "jolokia_password")
OX_COOKIE_SALT=$(derive_password "ox_appsuite" "cookie_hash_salt")
OX_SHARE_KEY=$(derive_password "ox_appsuite" "share_crypt_key")
OX_SESSIOND_KEY=$(derive_password "ox_appsuite" "sessiond_encryption_key")

_OX_EXISTING=$(curl -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
  "${BAO_ADDR}/v1/secret/data/gentian/${ENV}/ox" 2>/dev/null || true)
if echo "${_OX_EXISTING}" | grep -q '"data":{'; then
  echo "  Skipping secret/gentian/${ENV}/ox (already exists — use 'bao kv patch' to update)"
else
  # oidc_client_secret and connector_provisioning_api_password are random (not HMAC-derived)
  # so they can only be set on first creation; use 'bao kv patch' to rotate manually
  OX_OIDC_SECRET=$(openssl rand -hex 20)
  OX_CONNECTOR_PW=$(openssl rand -hex 20)
  kv_put "ox" "$(cat <<EOF
{
  "admin_password":                    "${OX_ADMIN_PW}",
  "hz_group_password":                 "${OX_HZ_GROUP_PW}",
  "basic_auth_password":               "${OX_BASIC_AUTH_PW}",
  "jolokia_password":                  "${OX_JOLOKIA_PW}",
  "cookie_hash_salt":                  "${OX_COOKIE_SALT}",
  "share_crypt_key":                   "${OX_SHARE_KEY}",
  "sessiond_encryption_key":           "${OX_SESSIOND_KEY}",
  "oidc_client_secret":                "${OX_OIDC_SECRET}",
  "connector_provisioning_api_password": "${OX_CONNECTOR_PW}"
}
EOF
)"
fi

# --- Dovecot ---
# doveadm_password: HMAC-derived for reproducibility
# oidc_client_secret: written by keycloak-config Tofu workspace (module "dovecot" in clients.tf)
#   If the keycloak-config workspace has already run, this kv_put_once is a no-op because
#   the secret already exists with oidc_client_secret.  In that case, patch manually:
#     bao kv patch secret/gentian/${ENV}/dovecot doveadm_password=<value>
_DOVECOT_EXISTING=$(curl -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
  "${BAO_ADDR}/v1/secret/data/gentian/${ENV}/dovecot" 2>/dev/null || true)
if echo "${_DOVECOT_EXISTING}" | grep -q '"doveadm_password"'; then
  echo "  Skipping secret/gentian/${ENV}/dovecot (doveadm_password already exists)"
elif echo "${_DOVECOT_EXISTING}" | grep -q '"data":{'; then
  # Secret exists (created by keycloak-config) but missing doveadm_password — patch it
  echo "  Patching secret/gentian/${ENV}/dovecot (adding doveadm_password)..."
  EXISTING_OIDC=$(echo "${_DOVECOT_EXISTING}" | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['data']['data']['oidc_client_secret'])" 2>/dev/null || echo "")
  if [ -n "${EXISTING_OIDC}" ]; then
    kv_put "dovecot" "$(cat <<EOF
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
  kv_put "dovecot" "$(cat <<EOF
{
  "doveadm_password":   "${DOVECOT_DOVEADM_PW}",
  "oidc_client_secret": "${DOVECOT_OIDC_SECRET}"
}
EOF
)"
fi

# --- Postfix (SMTP relay credentials) ---
if [ -n "$SMTP_RELAY_USER" ] && [ -n "$SMTP_RELAY_PASS" ]; then
    kv_put_once "postfix" "$(jq -n \
        --arg relay_username "${SMTP_RELAY_USER}" \
        --arg relay_password "${SMTP_RELAY_PASS}" \
        '{"relay_username": $relay_username, "relay_password": $relay_password}')"
else
    echo ""
    echo "  Skipping postfix credentials (not provided)."
    echo "  To add them later: bao kv put secret/gentian/${ENV}/postfix relay_username=<u> relay_password=<p>"
fi

# --- Registry (OCI pull credentials) — only if provided ---
if [ -n "$REGISTRY_USER" ] && [ -n "$REGISTRY_PASSWORD" ]; then
    kv_put_once "registry" "$(jq -n \
        --arg username "${REGISTRY_USER}" \
        --arg password "${REGISTRY_PASSWORD}" \
        '{"username": $username, "password": $password}')"
else
    echo ""
    echo "  Skipping registry credentials (not provided)."
    echo "  To add them later: bao kv put secret/gentian/${ENV}/registry username=<u> password=<p>"
fi

echo ""
echo "=========================================="
echo "✅ All secrets seeded into OpenBao!"
echo ""
echo "Verify with:"
echo "  bao kv list secret/gentian/${ENV}"
echo "  bao kv get secret/gentian/${ENV}/postgresql"
echo "=========================================="
