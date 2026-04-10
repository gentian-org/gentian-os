terraform {
  required_version = ">= 1.6"

  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
    keycloak = {
      source  = "mrparkers/keycloak"
      version = "~> 4.0"
    }
  }

  backend "s3" {
    bucket = "tofu-state"
    key    = "tenant/keycloak-config.tfstate"

    # MinIO endpoint (in-cluster)
    endpoint = "http://minio-dev.gentian-infra-dev.svc.cluster.local:9000"
    region   = "us-east-1"

    # MinIO does not expose the AWS metadata API
    force_path_style            = true
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}

# ── Providers ─────────────────────────────────────────────────────────────────

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

# Read the Keycloak admin password from OpenBao before constructing the
# Keycloak provider.  vault_kv_secret_v2 data sources are evaluated during the
# planning phase as long as the vault provider can authenticate.
data "vault_kv_secret_v2" "keycloak_admin" {
  mount = "secret"
  name  = "gentian-os/kernel/identity/keycloak-bootstrap"
}

# Per-app secrets — used to seed client_secret on initial client creation so
# the Keycloak client secret always matches the seed-derived OpenBao value.
data "vault_kv_secret_v2" "nextcloud" {
  mount = "secret"
  name  = "gentian-os/kernel/apps/nextcloud"
}

data "vault_kv_secret_v2" "intercom_secrets" {
  mount = "secret"
  name  = "gentian-os/kernel/identity/intercom"
}

data "vault_kv_secret_v2" "ox" {
  mount = "secret"
  name  = "gentian-os/kernel/apps/ox"
}

data "vault_kv_secret_v2" "dovecot_secrets" {
  mount = "secret"
  name  = "gentian-os/kernel/mail/dovecot"
}

data "vault_kv_secret_v2" "openproject" {
  mount = "secret"
  name  = "gentian-os/tenants/gtn-demo/apps/openproject/oidc"
}

locals {
  # Fall back to the computed in-cluster URL when keycloak_url is left empty.
  keycloak_url = var.keycloak_url != "" ? var.keycloak_url : "http://nubus-${var.env}-keycloak.gentian-${var.env}.svc.cluster.local:8080"
}

provider "keycloak" {
  url       = local.keycloak_url
  client_id = "admin-cli"
  username  = "kcadmin"
  password  = data.vault_kv_secret_v2.keycloak_admin.data["admin_password"]
  realm     = "master"
}
