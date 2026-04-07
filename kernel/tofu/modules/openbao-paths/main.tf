# =============================================================================
# openbao-paths — generates and writes all kernel secret paths
# =============================================================================
# Usage: call this module once per cluster to provision (or verify) the
# full OpenBao KV tree under secret/gentian-os/kernel/<category>/<component>.
#
# lifecycle { ignore_changes = [data_json] } is set on every vault_kv_secret_v2
# resource.  This means:
#   - First apply on a NEW cluster: creates secrets with generated random values.
#   - Subsequent applies on an EXISTING cluster: never overwrites live secrets.
#
# For an existing cluster that was seeded via seed-openbao.sh,
# import the live secrets into state before applying:
#
#   tofu import \
#     module.openbao_paths.vault_kv_secret_v2.postgresql \
#     "secret/gentian-os/kernel/database/postgresql"
#
# After import, Terraform knows the live values and lifecycle.ignore_changes
# prevents any drift.
# =============================================================================

terraform {
  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}

# ── Password generation ────────────────────────────────────────────────────────
# All passwords are 40-character hex strings (like SHA1 — matches existing
# values seeded by seed-openbao.sh).
# NATS passwords are prefixed with 'n' to guarantee they never start with a
# digit (upstream Nubus bug: NATS server rejects passwords starting with digits).

locals { pw_length = 40 }

resource "random_password" "pg_postgres" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_keycloak" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_keycloak_extensions" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_selfservice" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_authsession" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_guardian" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "pg_notifications" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "mariadb_root" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "mariadb_openxchange" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "redis_password" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "minio_root" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_nextcloud" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_openxchange" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_dovecot" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_migrations" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_notes" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_openproject" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "minio_ums" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "nubus_master" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_ldap_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_keycloak_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_ox_system_user" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_smtp" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_portal_shared" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_minio_ums_key" {
  length  = local.pw_length
  special = false
  upper   = false
}
# LDAP search users
resource "random_password" "ldap_keycloak" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_nextcloud" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_dovecot" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_element" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_ox" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_postfix" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_openproject" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ldap_xwiki" {
  length  = local.pw_length
  special = false
  upper   = false
}
# NATS — no digits allowed at position 0; prefix with 'n' below
resource "random_password" "nats_api" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nats_dispatcher" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nats_prefill" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nats_udm_listener" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nats_udm_transformer" {
  length  = local.pw_length
  special = false
  upper   = false
}
# PG passwords stored in nubus secret (nubus reads them from nubus-credentials ESO secret)
resource "random_password" "nubus_pg_selfservice" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_pg_authsession" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_pg_keycloak" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_pg_keycloak_ext" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_pg_guardian" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nubus_pg_notifications" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "keycloak_bootstrap_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "intercom_client_secret" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "intercom_oidc_secret" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "intercom_matrix_as" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "intercom_redis" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "intercom_portal_shared" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "intercom_session" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "nextcloud_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nextcloud_metrics" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "nextcloud_status" {
  length  = local.pw_length
  special = false
  upper   = false
}

# OX App Suite
resource "random_password" "ox_admin" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_hz_group" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_basic_auth" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_jolokia" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_cookie_hash_salt" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_share_crypt_key" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_sessiond_key" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_oidc_client_secret" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "ox_connector_api_password" {
  length  = local.pw_length
  special = false
  upper   = false
}

resource "random_password" "dovecot_doveadm" {
  length  = local.pw_length
  special = false
  upper   = false
}
resource "random_password" "dovecot_oidc_client_secret" {
  length  = local.pw_length
  special = false
  upper   = false
}

# ── KV secrets ────────────────────────────────────────────────────────────────

resource "vault_kv_secret_v2" "postgresql" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/database/postgresql"
  data_json = jsonencode({
    postgres_password                   = random_password.pg_postgres.result
    keycloak_user_password              = random_password.pg_keycloak.result
    keycloak_extensions_user_password   = random_password.pg_keycloak_extensions.result
    selfservice_user_password           = random_password.pg_selfservice.result
    authsession_user_password           = random_password.pg_authsession.result
    guardianmanagementapi_user_password = random_password.pg_guardian.result
    notificationsapi_user_password      = random_password.pg_notifications.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "mariadb" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/database/mariadb"
  data_json = jsonencode({
    root_password        = random_password.mariadb_root.result
    openxchange_password = random_password.mariadb_openxchange.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "redis" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/cache/redis"
  data_json = jsonencode({
    redis_password = random_password.redis_password.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "minio" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/storage/minio"
  data_json = jsonencode({
    root_password        = random_password.minio_root.result
    nextcloud_password   = random_password.minio_nextcloud.result
    openxchange_password = random_password.minio_openxchange.result
    dovecot_password     = random_password.minio_dovecot.result
    migrations_password  = random_password.minio_migrations.result
    notes_password       = random_password.minio_notes.result
    openproject_password = random_password.minio_openproject.result
    ums_password         = random_password.minio_ums.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "nubus" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/identity/nubus"
  data_json = jsonencode({
    master_password             = random_password.nubus_master.result
    admin_password              = random_password.nubus_admin.result
    ldap_admin_password         = random_password.nubus_ldap_admin.result
    keycloak_admin_password     = random_password.nubus_keycloak_admin.result
    ox_system_user_password     = random_password.nubus_ox_system_user.result
    smtp_password               = random_password.nubus_smtp.result
    portal_shared_secret        = random_password.nubus_portal_shared.result
    minio_ums_secret_access_key = random_password.nubus_minio_ums_key.result
    # LDAP search user passwords
    ldapsearch_keycloak    = random_password.ldap_keycloak.result
    ldapsearch_nextcloud   = random_password.ldap_nextcloud.result
    ldapsearch_dovecot     = random_password.ldap_dovecot.result
    ldapsearch_element     = random_password.ldap_element.result
    ldapsearch_ox          = random_password.ldap_ox.result
    ldapsearch_postfix     = random_password.ldap_postfix.result
    ldapsearch_openproject = random_password.ldap_openproject.result
    ldapsearch_xwiki       = random_password.ldap_xwiki.result
    # NATS passwords: must NOT start with digit — prefix with 'n'
    nats_api_password             = "n${random_password.nats_api.result}"
    nats_dispatcher_password      = "n${random_password.nats_dispatcher.result}"
    nats_prefill_password         = "n${random_password.nats_prefill.result}"
    nats_udm_listener_password    = "n${random_password.nats_udm_listener.result}"
    nats_udm_transformer_password = "n${random_password.nats_udm_transformer.result}"
    # PostgreSQL passwords for nubus components (stored here so nubus-credentials ESO reads one path)
    pg_selfservice_password         = random_password.nubus_pg_selfservice.result
    pg_authsession_password         = random_password.nubus_pg_authsession.result
    pg_keycloak_password            = random_password.nubus_pg_keycloak.result
    pg_keycloak_extensions_password = random_password.nubus_pg_keycloak_ext.result
    pg_guardian_password            = random_password.nubus_pg_guardian.result
    pg_notifications_password       = random_password.nubus_pg_notifications.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "keycloak_bootstrap" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/identity/keycloak-bootstrap"
  data_json = jsonencode({
    # Same password as nubus.keycloak_admin_password — Keycloak has one admin account
    admin_password         = random_password.keycloak_bootstrap_admin.result
    intercom_client_secret = random_password.intercom_client_secret.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "intercom" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/identity/intercom"
  data_json = jsonencode({
    oidc_client_secret   = random_password.intercom_oidc_secret.result
    matrix_as_token      = random_password.intercom_matrix_as.result
    redis_auth_password  = random_password.intercom_redis.result
    portal_shared_secret = random_password.intercom_portal_shared.result
    session_secret       = random_password.intercom_session.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "nextcloud" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/apps/nextcloud"
  data_json = jsonencode({
    admin_password  = random_password.nextcloud_admin.result
    metrics_token   = random_password.nextcloud_metrics.result
    status_password = random_password.nextcloud_status.result
  })
  lifecycle { ignore_changes = [data_json] }
}

# ── OX App Suite ──────────────────────────────────────────────────────────────
# All passwords match the openDesk derivePassword() scheme where possible so
# that they are reproducible from the master password if needed.

resource "vault_kv_secret_v2" "dovecot" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/mail/dovecot"
  data_json = jsonencode({
    doveadm_password   = random_password.dovecot_doveadm.result
    oidc_client_secret = random_password.dovecot_oidc_client_secret.result
  })
  lifecycle { ignore_changes = [data_json] }
}

resource "vault_kv_secret_v2" "ox" {
  mount = var.openbao_mount
  name  = "gentian-os/kernel/apps/ox"
  data_json = jsonencode({
    # OX master admin password (SOAP API + ox-connector auth)
    admin_password = random_password.ox_admin.result
    # Hazelcast cluster group password
    hz_group_password = random_password.ox_hz_group.result
    # BasicAuth and Jolokia passwords for internal OX admin endpoints
    basic_auth_password = random_password.ox_basic_auth.result
    jolokia_password    = random_password.ox_jolokia.result
    # Crypto keys — must be stable; rotation requires re-encrypting all OX data
    cookie_hash_salt        = random_password.ox_cookie_hash_salt.result
    share_crypt_key         = random_password.ox_share_crypt_key.result
    sessiond_encryption_key = random_password.ox_sessiond_key.result
    # Keycloak OIDC client secret for opendesk-oxappsuite
    oidc_client_secret = random_password.ox_oidc_client_secret.result
    # ox-connector → Nubus provisioning API auth password
    connector_provisioning_api_password = random_password.ox_connector_api_password.result
  })
  lifecycle { ignore_changes = [data_json] }
}
