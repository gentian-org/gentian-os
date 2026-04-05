# =============================================================================
# OX App Suite — three Helm releases with sensitive values from OpenBao
# =============================================================================
# Pattern B: OX uses a templatefile() approach for secrets because the deeply
# nested configFiles / propertiesFiles keys (with dots and slashes) cannot be
# passed cleanly via set_sensitive. The rendered YAML template is passed as a
# sensitive value in the `values` list.
#
# Install order (enforced by depends_on):
#   1. open-xchange                      — main OX App Suite middleware
#   2. opendesk-open-xchange-bootstrap   — context/filestore bootstrap job
#   3. ox-connector                      — Nubus provisioning ↔ OX sync
#
# Charts from:
#   OX main:      oci://registry.opencode.de/.../open-xchange/charts-mirror
#   Bootstrap:    oci://registry.opencode.de/.../platform-development/charts
#   OX Connector: oci://registry.opencode.de/.../univention/charts-mirror
# =============================================================================

# ── Data sources ──────────────────────────────────────────────────────────────

data "vault_kv_secret_v2" "ox" {
  mount = "secret"
  name  = "gentian/${var.env}/ox"
}

data "vault_kv_secret_v2" "minio" {
  mount = "secret"
  name  = "gentian/${var.env}/minio"
}

data "vault_kv_secret_v2" "redis_ox" {
  mount = "secret"
  name  = "gentian/${var.env}/redis"
}

# Note: data.vault_kv_secret_v2.mariadb  is defined in stubs.tf
# Note: data.vault_kv_secret_v2.nubus    is defined in data.tf

# ── 1. open-xchange (OX App Suite main) ──────────────────────────────────────
# Release name "open-xchange" (not "open-xchange-${var.env}") to match the
# service naming convention expected by the bootstrap and connector charts:
#   open-xchange-core-mw-admin.gentian-${var.env}.svc.cluster.local

resource "helm_release" "ox_appsuite" {
  name       = "open-xchange"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/supplier/open-xchange/charts-mirror"
  chart      = "appsuite-public-sector"
  version    = "2.26.32"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 1200

  values = [
    file("${path.module}/../../../apps/ox-appsuite/values/_base.yaml"),
    file("${path.module}/../../../apps/ox-appsuite/values/${var.env}/values-plain.yaml"),
    sensitive(templatefile(
      "${path.module}/../../../apps/ox-appsuite/values/${var.env}/values-sensitive.yaml.tftpl",
      {
        # OX-specific secrets
        admin_password          = data.vault_kv_secret_v2.ox.data["admin_password"]
        hz_group_password       = data.vault_kv_secret_v2.ox.data["hz_group_password"]
        basic_auth_password     = data.vault_kv_secret_v2.ox.data["basic_auth_password"]
        jolokia_password        = data.vault_kv_secret_v2.ox.data["jolokia_password"]
        cookie_hash_salt        = data.vault_kv_secret_v2.ox.data["cookie_hash_salt"]
        share_crypt_key         = data.vault_kv_secret_v2.ox.data["share_crypt_key"]
        sessiond_encryption_key = data.vault_kv_secret_v2.ox.data["sessiond_encryption_key"]
        oidc_client_secret      = data.vault_kv_secret_v2.ox.data["oidc_client_secret"]
        # Shared infrastructure secrets
        mariadb_root_password = data.vault_kv_secret_v2.mariadb.data["root_password"]
        minio_ox_password     = data.vault_kv_secret_v2.minio.data["openxchange_password"]
        ldap_ox_password      = data.vault_kv_secret_v2.nubus.data["ldapsearch_ox"]
        redis_auth_password   = data.vault_kv_secret_v2.redis_ox.data["auth_password"]
      }
    )),
  ]
}

# ── 2. opendesk-open-xchange-bootstrap ───────────────────────────────────────
# One-shot Job that executes OX bootstrap commands (context creation, filestore
# registration) via kubectl exec into the core-mw admin pod.
# Must run after the main OX deployment is fully ready.

resource "helm_release" "ox_bootstrap" {
  depends_on = [helm_release.ox_appsuite]

  name       = "opendesk-open-xchange-bootstrap"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-open-xchange-bootstrap"
  chart      = "opendesk-open-xchange-bootstrap"
  version    = "4.0.2"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 600

  # Filestore size in MB — reduced for dev (10 GB instead of default 100 GB)
  set {
    name  = "filestore.size"
    value = "10000"
  }
}

# ── 3. ox-connector ──────────────────────────────────────────────────────────
# Listens on the Nubus UDM provisioning API and syncs users/groups into OX.
# Connects to OX via the admin SOAP service.
# Requires the Secret "ox-connector-provisioning-api" (created by ESO) to be
# present before nubus re-deploys with apps.oxAppSuite.enabled=true.

resource "helm_release" "ox_connector" {
  depends_on = [helm_release.ox_appsuite]

  name       = "ox-connector"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/supplier/univention/charts-mirror"
  chart      = "ox-connector"
  version    = "0.34.0"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 600

  set {
    name  = "openXchange.domainName"
    value = "desk.gentian.org"
  }

  set {
    name  = "openXchange.auth.username"
    value = "admin"
  }

  set {
    name  = "openXchange.oxDefaultContext"
    value = "1"
  }

  set {
    name  = "openXchange.oxSoapServer"
    value = "http://open-xchange-core-mw-admin.gentian-${var.env}.svc.cluster.local"
  }

  set {
    name  = "openXchange.oxLocalTimezone"
    value = "Europe/Berlin"
  }

  set {
    name  = "openXchange.oxLanguage"
    value = "de_DE"
  }

  set {
    name  = "openXchange.oxSmtpServer"
    value = "smtp://postfix-${var.env}.gentian-${var.env}.svc.cluster.local:587"
  }

  set {
    name  = "openXchange.oxImapServer"
    value = "imap://opendesk-dovecot-${var.env}.gentian-${var.env}.svc.cluster.local:143"
  }

  set {
    name  = "provisioningApi.connection.baseUrl"
    value = "http://nubus-${var.env}-provisioning-api.gentian-${var.env}.svc.cluster.local"
  }

  set {
    name  = "provisioningApi.auth.username"
    value = "ox-connector"
  }

  set_sensitive {
    name  = "openXchange.auth.password"
    value = data.vault_kv_secret_v2.ox.data["admin_password"]
  }

  # Use existing ESO-managed secret for provisioning API password.
  # The "ox-connector-provisioning-api" secret is created by ExternalSecret (ESO)
  # and also contains the "registration" JSON consumed by Nubus. Referencing it
  # as existingSecret avoids Helm trying to create/own this ESO-owned secret.
  set {
    name  = "provisioningApi.auth.existingSecret.name"
    value = "ox-connector-provisioning-api"
  }
}
