# =============================================================================
# Nextcloud — three Helm releases with sensitive values from OpenBao
# =============================================================================
# Pattern B: all credentials injected via set_sensitive.
# Neither opendesk-nextcloud nor opendesk-nextcloud-management support
# existingSecret for DB/cache/LDAP/OIDC credentials.
#
# Install order (enforced by depends_on):
#   1. nextcloud-management  — bootstraps the Nextcloud instance (runs jobs)
#   2. nextcloud             — the main AIO workload
#   3. nextcloud-notifypush  — notify-push sidecar (depends on AIO being up)
#
# Charts from:
#   oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-nextcloud
# =============================================================================

# ── Data sources ──────────────────────────────────────────────────────────────

data "vault_kv_secret_v2" "nextcloud" {
  mount = "secret"
  name  = "gentian/${var.env}/nextcloud"
}

data "vault_kv_secret_v2" "redis_nc" {
  mount = "secret"
  name  = "gentian/${var.env}/redis"
}

data "vault_kv_secret_v2" "minio_nc" {
  mount = "secret"
  name  = "gentian/${var.env}/minio"
}

data "vault_kv_secret_v2" "nubus_nc" {
  mount = "secret"
  name  = "gentian/${var.env}/nubus"
}

# ── 1. opendesk-nextcloud-management ─────────────────────────────────────────
# Must run before the AIO deployment — provisions the Nextcloud instance,
# configures apps, LDAP, etc.  wait=true because AIO depends on it.

resource "helm_release" "nextcloud_management" {
  name       = "nextcloud-management-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-nextcloud"
  chart      = "opendesk-nextcloud-management"
  version    = "4.7.2"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 900

  values = [
    file("${path.module}/../../../apps/nextcloud-management/values/_base.yaml"),
    file("${path.module}/../../../apps/nextcloud-management/values/${var.env}/values-plain.yaml"),
  ]

  # Nextcloud admin account
  set_sensitive {
    name  = "configuration.administrator.password.value"
    value = data.vault_kv_secret_v2.nextcloud.data["admin_password"]
  }

  # PostgreSQL nextcloud_user password
  set_sensitive {
    name  = "configuration.database.auth.password.value"
    value = data.vault_kv_secret_v2.postgresql.data["nextcloud_user_password"]
  }

  # Redis auth password
  set_sensitive {
    name  = "configuration.cache.auth.password.value"
    value = data.vault_kv_secret_v2.redis_nc.data["auth_password"]
  }

  # LDAP search user (ldapsearch_nextcloud)
  set_sensitive {
    name  = "configuration.ldap.password.value"
    value = data.vault_kv_secret_v2.nubus_nc.data["ldapsearch_nextcloud"]
  }

  # MinIO object store (nextcloud bucket)
  set_sensitive {
    name  = "configuration.objectstore.auth.secretKey.value"
    value = data.vault_kv_secret_v2.minio_nc.data["nextcloud_password"]
  }

  # OIDC client secret
  set_sensitive {
    name  = "configuration.oidc.password.value"
    value = data.vault_kv_secret_v2.nextcloud.data["oidc_client_secret"]
  }

  # Central navigation API key (opendesk integration)
  set_sensitive {
    name  = "configuration.opendeskIntegration.centralNavigation.password.value"
    value = data.vault_kv_secret_v2.nubus_nc.data["portal_shared_secret"]
  }

  # Metrics / serverinfo token
  set_sensitive {
    name  = "configuration.serverinfo.token.value"
    value = data.vault_kv_secret_v2.nextcloud.data["metrics_token"]
  }

  # SMTP password (postfix opendesk-system user)
  set_sensitive {
    name  = "configuration.smtp.auth.password.value"
    value = data.vault_kv_secret_v2.nubus_nc.data["smtp_password"]
  }

  # Workaround: chart v4.7.2 bug — ldapAgentPassword is expected inside the
  # FS_ENV_LDAP JSON but the ldap-config.json.gotmpl template omits it.
  # Override FS_ENV_LDAP entirely (appended after the hardcoded chart value,
  # so the last definition wins in the container env) with a complete JSON
  # that includes ldapAgentPassword and the correct single ldap:// host prefix.
  set_sensitive {
    name  = "extraEnvVars[1].name"
    value = "FS_ENV_LDAP"
  }
  set_sensitive {
    name  = "extraEnvVars[1].value"
    value = base64encode(jsonencode({
      "ldapAgentPassword"          = data.vault_kv_secret_v2.nubus_nc.data["ldapsearch_nextcloud"]
      "ldapAgentName"              = "uid=ldapsearch_nextcloud,cn=users,dc=swp-ldap,dc=internal"
      "ldapBase"                   = "dc=swp-ldap,dc=internal"
      "ldapBaseGroups"             = "dc=swp-ldap,dc=internal"
      "ldapBaseUsers"              = "dc=swp-ldap,dc=internal"
      "ldapHost"                   = "ldap://nubus-${var.env}-ldap-server-primary.gentian-${var.env}.svc.cluster.local"
      "ldapPort"                   = "389"
      "ldapAdminGroup"             = "managed-by-attribute-FileshareAdmin"
      "ldapGroupDisplayName"       = "cn"
      "ldapGroupFilter"            = "'(&(objectClass=opendeskFileshareGroup)(opendeskFileshareEnabled=TRUE))'"
      "ldapGroupFilterObjectclass" = "opendeskFileshareGroup"
      "ldapLoginFilter"            = "'(&(opendeskFileshareEnabled=TRUE)(entryUUID=%uid))'"
      "ldapUserFilter"             = "'(opendeskFileshareEnabled=TRUE)'"
      "ldapUserFilterObjectclass"  = "opendeskFileshareUser"
    }))
  }
}

# ── 2. opendesk-nextcloud (AIO) ───────────────────────────────────────────────

resource "helm_release" "nextcloud" {
  depends_on = [helm_release.nextcloud_management]

  name       = "nextcloud-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-nextcloud"
  chart      = "opendesk-nextcloud"
  version    = "4.7.2"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = false # long-running init; notifypush depends_on handles ordering
  timeout          = 900

  values = [
    file("${path.module}/../../../apps/nextcloud/values/_base.yaml"),
    file("${path.module}/../../../apps/nextcloud/values/${var.env}/values-plain.yaml"),
  ]

  # PostgreSQL nextcloud_user password
  set_sensitive {
    name  = "aio.configuration.database.auth.password.value"
    value = data.vault_kv_secret_v2.postgresql.data["nextcloud_user_password"]
  }

  # Redis auth password — no longer injected here; the AIO chart now reads from
  # the nextcloud-redis-cache ExternalSecret (apps/nextcloud/secrets/dev/externalsecret.yaml)
  # which syncs from the same Vault key. Reloader restarts the pod on ESO refresh.

  # Status PHP password (used by the metrics exporter)
  set_sensitive {
    name  = "aio.configuration.statusPhp.password.value"
    value = data.vault_kv_secret_v2.nextcloud.data["status_password"]
  }

  # Metrics / serverinfo token (also used by the built-in exporter)
  set_sensitive {
    name  = "exporter.configuration.token.value"
    value = data.vault_kv_secret_v2.nextcloud.data["metrics_token"]
  }
}

# ── 3. opendesk-nextcloud-notifypush ─────────────────────────────────────────

resource "helm_release" "nextcloud_notifypush" {
  depends_on = [helm_release.nextcloud]

  name       = "nextcloud-notifypush-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-nextcloud"
  chart      = "opendesk-nextcloud-notifypush"
  version    = "4.7.2"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file("${path.module}/../../../apps/nextcloud-notifypush/values/_base.yaml"),
    file("${path.module}/../../../apps/nextcloud-notifypush/values/${var.env}/values-plain.yaml"),
  ]

  # PostgreSQL nextcloud_user password
  set_sensitive {
    name  = "configuration.database.auth.password.value"
    value = data.vault_kv_secret_v2.postgresql.data["nextcloud_user_password"]
  }

  # Redis auth password
  set_sensitive {
    name  = "configuration.cache.auth.password.value"
    value = data.vault_kv_secret_v2.redis_nc.data["auth_password"]
  }
}
