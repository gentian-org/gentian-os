# =============================================================================
# stubs.tf — helm_release resources for platform-managed infrastructure charts
# =============================================================================
# postgresql and mariadb are openDesk platform-development charts.  They moved
# from the now-empty supplier/univention/charts-mirror OCI path to:
#   registry.opencode.de/bmi/opendesk/components/platform-development/charts/
# (confirmed active in openDesk develop branch charts.yaml.gotmpl, 2026-03-05)
#
# keycloak-bootstrap is DEPRECATED in our architecture — Keycloak client
# configuration is managed declaratively by tofu/tenant/keycloak-config via
# the mrparkers/keycloak Terraform provider.  Chart path is updated for
# reference but the resource stays at count = 0.
# =============================================================================

# ── Data sources ──────────────────────────────────────────────────────────────

data "vault_kv_secret_v2" "postgresql" {
  mount = "secret"
  name  = "gentian-os/kernel/database/postgresql"
}

data "vault_kv_secret_v2" "mariadb" {
  mount = "secret"
  name  = "gentian-os/kernel/database/mariadb"
}

data "vault_kv_secret_v2" "keycloak_bootstrap" {
  mount = "secret"
  name  = "gentian-os/kernel/identity/keycloak-bootstrap"
}

# ── opendesk-postgresql ───────────────────────────────────────────────────────
# NOTE: count = 0 — the active PostgreSQL is the ArgoCD-managed `postgresql-dev`
# StatefulSet in gentian-infra-dev (pre-Tofu deployment; namespace = gentian-infra-dev).
# When ready to migrate: set count = 1, run `tofu import`, transfer data with pg_dump,
# then delete the old StatefulSet.  The namespace is pre-set correctly below.

resource "helm_release" "postgresql" {
  count = 1  # migration active — replaces legacy postgresql-dev StatefulSet

  name       = "opendesk-postgresql-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-postgresql"
  chart      = "postgresql"
  version    = "2.1.2"
  namespace  = "gentian-infra-${var.env}"  # infra tier: gentian-infra-{env}

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file("${path.module}/../../../apps/postgresql/values/_base.yaml"),
    file("${path.module}/../../../apps/postgresql/values/${var.env}/values-plain.yaml"),
  ]

  set_sensitive {
    name  = "postgres.password"
    value = data.vault_kv_secret_v2.postgresql.data["postgres_password"]
  }
  # Injecting passwords for job.users[] — order must match apps/postgresql/values/dev/values-plain.yaml
  set_sensitive {
    name  = "job.users[0].password"
    value = data.vault_kv_secret_v2.postgresql.data["keycloak_user_password"]
  }
  set_sensitive {
    name  = "job.users[1].password"
    value = data.vault_kv_secret_v2.postgresql.data["keycloak_extensions_user_password"]
  }
  set_sensitive {
    name  = "job.users[2].password"
    value = data.vault_kv_secret_v2.postgresql.data["selfservice_user_password"]
  }
  set_sensitive {
    name  = "job.users[3].password"
    value = data.vault_kv_secret_v2.postgresql.data["authsession_user_password"]
  }
  set_sensitive {
    name  = "job.users[4].password"
    value = data.vault_kv_secret_v2.postgresql.data["guardianmanagementapi_user_password"]
  }
  set_sensitive {
    name  = "job.users[5].password"
    value = data.vault_kv_secret_v2.postgresql.data["notificationsapi_user_password"]
  }
  set_sensitive {
    name  = "job.users[6].password"
    value = data.vault_kv_secret_v2.postgresql.data["nextcloud_user_password"]
  }
}

# ── opendesk-mariadb ──────────────────────────────────────────────────────────
# NOTE: count = 0 — same migration pattern as postgresql above.
# When ready to migrate: set count = 1, run `tofu import`, transfer data with mysqldump,
# then delete the old StatefulSet.  The namespace is pre-set correctly below.

resource "helm_release" "mariadb" {
  count = 1  # migration active — replaces legacy mariadb-dev StatefulSet

  name       = "opendesk-mariadb-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-mariadb"
  chart      = "mariadb"
  version    = "3.0.3"
  namespace  = "gentian-infra-${var.env}"  # infra tier: gentian-infra-{env}

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file("${path.module}/../../../apps/mariadb/values/_base.yaml"),
    file("${path.module}/../../../apps/mariadb/values/${var.env}/values-plain.yaml"),
  ]

  set_sensitive {
    name  = "mariadb.rootPassword.value"
    value = data.vault_kv_secret_v2.mariadb.data["root_password"]
  }

  set_sensitive {
    name  = "job.users[0].password"
    value = data.vault_kv_secret_v2.mariadb.data["openxchange_password"]
  }
}

# ── opendesk-keycloak-bootstrap ───────────────────────────────────────────────
# DEPRECATED — replaced by tofu/tenant/keycloak-config (mrparkers/keycloak
# provider).  Chart path updated for reference; resource stays at count = 0.

resource "helm_release" "keycloak_bootstrap" {
  count = 0 # DEPRECATED — use tofu/tenant/keycloak-config instead

  name       = "opendesk-keycloak-bootstrap-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-keycloak-bootstrap"
  chart      = "opendesk-keycloak-bootstrap"
  version    = "2.7.1"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file("${path.module}/../../../apps/keycloak-bootstrap/values/_base.yaml"),
    file("${path.module}/../../../apps/keycloak-bootstrap/values/${var.env}/values-plain.yaml"),
  ]

  set_sensitive {
    name  = "keycloak.auth.adminPassword"
    value = data.vault_kv_secret_v2.keycloak_bootstrap.data["admin_password"]
  }
  set_sensitive {
    name  = "intercom.oidc.clientSecret"
    value = data.vault_kv_secret_v2.keycloak_bootstrap.data["intercom_client_secret"]
  }
}
