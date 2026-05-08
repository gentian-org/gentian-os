# =============================================================================
# udm-config — UDM REST API workspace for declarative user/group provisioning
# =============================================================================
# Manages users and groups in the Nubus LDAP directory via the UDM REST API.
# This is the authoritative interface for identity provisioning in the Nubus
# stack: UDM writes to LDAP, and Keycloak federates from LDAP.  Creating
# identities here ensures they appear correctly in both LDAP and Keycloak.
#
# NOTE on drift detection:
#   UDM GET responses contain ~150 auto-set attributes (ox*, opendesk*,
#   samba*, gidNumber, etc.) that are not part of the managed spec.  User
#   resources use lifecycle { ignore_changes = [data] } to avoid spurious
#   replacements — Terraform creates/deletes users but does NOT auto-reconcile
#   property drift.  To force a property update: terraform taint <resource>.
#
# Providers:
#   - hashicorp/vault  : reads admin credentials from OpenBao
#   - Mastercard/restapi: CRUD operations against the UDM REST API
#
# Backend: S3-compatible MinIO (in-cluster)
# =============================================================================

terraform {
  required_version = ">= 1.6"

  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
    restapi = {
      source  = "Mastercard/restapi"
      version = "~> 1.20"
    }
  }

  backend "s3" {
    bucket = "tofu-state"
    key    = "tenant/udm-config.tfstate"

    # MinIO endpoint (in-cluster)
    endpoint = "http://minio-dev.gentian-infra-dev.svc.cluster.local:9000"
    region   = "us-east-1"

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

# Read the Nubus admin password from OpenBao.
data "vault_kv_secret_v2" "nubus" {
  mount = "secret"
  name  = "gentian-os/kernel/identity/nubus"
}

provider "restapi" {
  # UDM REST API is exposed through the Nubus portal ingress.
  # In-cluster path: https://portal.{domain}/univention/udm/
  uri      = "https://portal.${var.domain}"
  username = "Administrator"
  password = data.vault_kv_secret_v2.nubus.data["admin_password"]

  # POST returns only {"dn": "...", "uuid": "...", "_links": {...}}.
  # The provider must do a separate GET to populate state after creation.
  write_returns_object  = false
  create_returns_object = false

  # Verify the UDM API is reachable before any resource operations.
  test_path = "/univention/udm/"

  headers = {
    "Content-Type" = "application/json"
    "Accept"       = "application/json"
  }

  # Ignore TLS certificate errors for self-signed cluster certificates.
  # Set to false once a proper CA is in place.
  insecure = true
}
