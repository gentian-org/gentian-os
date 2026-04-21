# =============================================================================
# app-workspace — generic Pattern B app deployer
# =============================================================================
# Deploys a single Helm chart per tenant app using sensitive values read from
# OpenBao via the vault provider. Credentials never appear in Terraform state
# or ArgoCD Application CRs — they are injected via set_sensitive at apply time.
#
# Each gentian-os Tenant that declares a Pattern B app (deploymentMethod:
# tofu-controller) gets one Terraform CR that points to this module. The
# operator passes the chart reference and valueMapping keys via spec.vars.
#
# OpenBao secret layout (KV v2 mount "secret"):
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/oidc
#     issuer, client-id, client-secret
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/database
#     host, port, name, user, password
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/s3
#     endpoint, bucket, access-key, secret-key, region
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/cache
#     host, port, password
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/smtp
#     host, port, user, password
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/imap
#     host, port
#   gentian-os/tenants/{tenant_name}/apps/{app_name}/ldap
#     host, port, base-dn, bind-dn, bind-password
# =============================================================================

terraform {
  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.0"
    }
  }

  backend "s3" {
    # Bucket and key are parameterised at init time via -backend-config when the
    # Tofu Controller initialises the workspace. The MinIO endpoint is injected via
    # the minio-tofu-state Secret referenced in the Terraform CR's envFrom.
    bucket = "tofu-state"
    key    = "placeholder" # overridden at workspace init

    endpoint         = "http://minio-dev.gentian-infra-dev.svc.cluster.local:9000"
    region           = "us-east-1"
    force_path_style = true

    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}

# ── OpenBao (Vault) provider: Kubernetes auth via runner SA ───────────────────

provider "vault" {
  address = "http://openbao.openbao.svc.cluster.local:8200"

  auth_login {
    path = "auth/kubernetes/login"
    parameters = {
      role = "tofu-runner"
      jwt  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
    }
  }
}

# ── Helm provider: in-cluster credentials ─────────────────────────────────────

provider "helm" {
  kubernetes {
    host                   = "https://kubernetes.default.svc"
    token                  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
    cluster_ca_certificate = file("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
  }

  dynamic "registry" {
    for_each = var.registry_username != "" ? [1] : []
    content {
      url      = "oci://${split("/", trimprefix(var.chart_repository, "oci://"))[0]}"
      username = var.registry_username
      password = var.registry_password
    }
  }
}

# ── Local computed paths ───────────────────────────────────────────────────────

locals {
  secret_base = "gentian-os/tenants/${var.tenant_name}/apps/${var.app_name}"

  # Determine which service categories are needed based on the vm_* variables.
  need_oidc     = anytrue([var.vm_oidc_issuer_key != "", var.vm_oidc_client_id_key != "", var.vm_oidc_client_secret_key != ""])
  need_database = anytrue([var.vm_db_host_key != "", var.vm_db_port_key != "", var.vm_db_name_key != "", var.vm_db_user_key != "", var.vm_db_password_key != ""])
  need_s3       = anytrue([var.vm_s3_endpoint_key != "", var.vm_s3_bucket_key != "", var.vm_s3_access_key_key != "", var.vm_s3_secret_key_key != "", var.vm_s3_region_key != ""])
  need_cache    = anytrue([var.vm_cache_host_key != "", var.vm_cache_port_key != "", var.vm_cache_password_key != ""])
  need_smtp     = anytrue([var.vm_smtp_host_key != "", var.vm_smtp_port_key != "", var.vm_smtp_user_key != "", var.vm_smtp_password_key != ""])
  need_imap     = anytrue([var.vm_imap_host_key != "", var.vm_imap_port_key != ""])
  need_ldap     = anytrue([var.vm_ldap_host_key != "", var.vm_ldap_port_key != "", var.vm_ldap_base_dn_key != "", var.vm_ldap_bind_dn_key != "", var.vm_ldap_bind_password_key != ""])
}

# ── OpenBao data sources (one per service category, on-demand) ────────────────

data "vault_kv_secret_v2" "oidc" {
  count = local.need_oidc ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/oidc"
}

data "vault_kv_secret_v2" "database" {
  count = local.need_database ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/database"
}

data "vault_kv_secret_v2" "s3" {
  count = local.need_s3 ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/s3"
}

data "vault_kv_secret_v2" "cache" {
  count = local.need_cache ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/cache"
}

data "vault_kv_secret_v2" "smtp" {
  count = local.need_smtp ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/smtp"
}

data "vault_kv_secret_v2" "imap" {
  count = local.need_imap ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/imap"
}

data "vault_kv_secret_v2" "ldap" {
  count = local.need_ldap ? 1 : 0
  mount = "secret"
  name  = "${local.secret_base}/ldap"
}

# App-internal secrets (Inc 21a) — one data source per AppProfile.appSecrets entry.
# The operator fills var.app_secrets with {name = valuePath}; each name is stored
# at {secret_base}/internal/{name} with a single "value" key.
data "vault_kv_secret_v2" "app_secret" {
  for_each = var.app_secrets
  mount    = "secret"
  name     = "${local.secret_base}/internal/${each.key}"
}

# ── Sensitive value map ────────────────────────────────────────────────────────
# Keys = Helm value paths (not sensitive — from vm_* variables).
# Values = credentials read from OpenBao (sensitive).
# The set_sensitive dynamic block injects each entry into the Helm release.

locals {
  sensitive_values = merge(
    # OIDC
    var.vm_oidc_issuer_key != "" && local.need_oidc ? {
      (var.vm_oidc_issuer_key) = data.vault_kv_secret_v2.oidc[0].data["issuer"]
    } : {},
    var.vm_oidc_client_id_key != "" && local.need_oidc ? {
      (var.vm_oidc_client_id_key) = data.vault_kv_secret_v2.oidc[0].data["client-id"]
    } : {},
    var.vm_oidc_client_secret_key != "" && local.need_oidc ? {
      (var.vm_oidc_client_secret_key) = data.vault_kv_secret_v2.oidc[0].data["client-secret"]
    } : {},

    # Database
    var.vm_db_host_key != "" && local.need_database ? {
      (var.vm_db_host_key) = data.vault_kv_secret_v2.database[0].data["host"]
    } : {},
    var.vm_db_port_key != "" && local.need_database ? {
      (var.vm_db_port_key) = data.vault_kv_secret_v2.database[0].data["port"]
    } : {},
    var.vm_db_name_key != "" && local.need_database ? {
      (var.vm_db_name_key) = data.vault_kv_secret_v2.database[0].data["name"]
    } : {},
    var.vm_db_user_key != "" && local.need_database ? {
      (var.vm_db_user_key) = data.vault_kv_secret_v2.database[0].data["user"]
    } : {},
    var.vm_db_password_key != "" && local.need_database ? {
      (var.vm_db_password_key) = data.vault_kv_secret_v2.database[0].data["password"]
    } : {},

    # S3 / object storage
    var.vm_s3_endpoint_key != "" && local.need_s3 ? {
      (var.vm_s3_endpoint_key) = data.vault_kv_secret_v2.s3[0].data["endpoint"]
    } : {},
    var.vm_s3_bucket_key != "" && local.need_s3 ? {
      (var.vm_s3_bucket_key) = data.vault_kv_secret_v2.s3[0].data["bucket"]
    } : {},
    var.vm_s3_access_key_key != "" && local.need_s3 ? {
      (var.vm_s3_access_key_key) = data.vault_kv_secret_v2.s3[0].data["access-key"]
    } : {},
    var.vm_s3_secret_key_key != "" && local.need_s3 ? {
      (var.vm_s3_secret_key_key) = data.vault_kv_secret_v2.s3[0].data["secret-key"]
    } : {},
    var.vm_s3_region_key != "" && local.need_s3 ? {
      (var.vm_s3_region_key) = data.vault_kv_secret_v2.s3[0].data["region"]
    } : {},

    # Cache (Redis/Memcached)
    var.vm_cache_host_key != "" && local.need_cache ? {
      (var.vm_cache_host_key) = data.vault_kv_secret_v2.cache[0].data["host"]
    } : {},
    var.vm_cache_port_key != "" && local.need_cache ? {
      (var.vm_cache_port_key) = data.vault_kv_secret_v2.cache[0].data["port"]
    } : {},
    var.vm_cache_password_key != "" && local.need_cache ? {
      (var.vm_cache_password_key) = data.vault_kv_secret_v2.cache[0].data["password"]
    } : {},

    # SMTP
    var.vm_smtp_host_key != "" && local.need_smtp ? {
      (var.vm_smtp_host_key) = data.vault_kv_secret_v2.smtp[0].data["host"]
    } : {},
    var.vm_smtp_port_key != "" && local.need_smtp ? {
      (var.vm_smtp_port_key) = data.vault_kv_secret_v2.smtp[0].data["port"]
    } : {},
    var.vm_smtp_user_key != "" && local.need_smtp ? {
      (var.vm_smtp_user_key) = data.vault_kv_secret_v2.smtp[0].data["user"]
    } : {},
    var.vm_smtp_password_key != "" && local.need_smtp ? {
      (var.vm_smtp_password_key) = data.vault_kv_secret_v2.smtp[0].data["password"]
    } : {},

    # IMAP
    var.vm_imap_host_key != "" && local.need_imap ? {
      (var.vm_imap_host_key) = data.vault_kv_secret_v2.imap[0].data["host"]
    } : {},
    var.vm_imap_port_key != "" && local.need_imap ? {
      (var.vm_imap_port_key) = data.vault_kv_secret_v2.imap[0].data["port"]
    } : {},

    # LDAP
    var.vm_ldap_host_key != "" && local.need_ldap ? {
      (var.vm_ldap_host_key) = data.vault_kv_secret_v2.ldap[0].data["host"]
    } : {},
    var.vm_ldap_port_key != "" && local.need_ldap ? {
      (var.vm_ldap_port_key) = data.vault_kv_secret_v2.ldap[0].data["port"]
    } : {},
    var.vm_ldap_base_dn_key != "" && local.need_ldap ? {
      (var.vm_ldap_base_dn_key) = data.vault_kv_secret_v2.ldap[0].data["base-dn"]
    } : {},
    var.vm_ldap_bind_dn_key != "" && local.need_ldap ? {
      (var.vm_ldap_bind_dn_key) = data.vault_kv_secret_v2.ldap[0].data["bind-dn"]
    } : {},
    var.vm_ldap_bind_password_key != "" && local.need_ldap ? {
      (var.vm_ldap_bind_password_key) = data.vault_kv_secret_v2.ldap[0].data["bind-password"]
    } : {},

    # App-internal secrets (Inc 21a) — map{name → valuePath} × read{name → value}.
    {
      for name, path in var.app_secrets :
      path => data.vault_kv_secret_v2.app_secret[name].data["value"]
    },
  )
}

# ── Helm release ───────────────────────────────────────────────────────────────

resource "helm_release" "app" {
  name       = var.app_name
  repository = var.chart_repository
  chart      = var.chart_name
  version    = var.chart_version
  namespace  = var.namespace

  create_namespace = false
  wait             = true
  timeout          = 600
  # replace=true maps to helm install --replace, which handles the case where
  # the release already exists in a deployed state when Terraform has no prior
  # state (e.g. first run after backend migration). With persistent state,
  # Terraform uses helm upgrade on subsequent runs and this flag has no effect.
  replace = true

  # Non-sensitive extra values from the AppProfile (extraValues + replica overrides).
  values = var.extra_values_json != "" ? [var.extra_values_json] : []

  # Sensitive values from OpenBao, injected via set_sensitive so they never
  # appear in Terraform state, plan output, or the ArgoCD UI.
  dynamic "set_sensitive" {
    for_each = local.sensitive_values
    content {
      name  = set_sensitive.key
      value = set_sensitive.value
    }
  }
}
