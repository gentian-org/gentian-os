# =============================================================================
# openbao-tenant-policy — per-tenant read-only OpenBao policy
# =============================================================================
# Call this module once per tenant to create:
#   - A read-only policy scoped to gentian-os/tenants/{tenant}/apps/...
#   - A Kubernetes auth backend role so tenant pods can authenticate with
#     their own ServiceAccount and receive a token limited to their paths.
#
# Usage (e.g. in a per-tenant Tofu workspace or from the orchestrator):
#
#   module "acme_policy" {
#     source      = "../../modules/openbao-tenant-policy"
#     tenant_name = "acme-corp"
#   }
# =============================================================================

terraform {
  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
  }
}

# Read-only policy: a tenant can read all secrets under its own path tree.
resource "vault_policy" "tenant_read" {
  name   = "tenant-read-${var.tenant_name}"
  policy = <<-EOT
    # Read-only access to all app secrets for tenant ${var.tenant_name}.
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/oidc" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/database" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/s3" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/ldap" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/smtp" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/imap" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/apps/*/cache" {
      capabilities = ["read"]
    }
    path "secret/data/gentian-os/tenants/${var.tenant_name}/contracts/*" {
      capabilities = ["read"]
    }
    path "secret/metadata/gentian-os/tenants/${var.tenant_name}/*" {
      capabilities = ["list"]
    }
  EOT
}

# Kubernetes auth role: binds the policy to pods running in the tenant namespace.
resource "vault_kubernetes_auth_backend_role" "tenant" {
  backend   = var.kubernetes_auth_path
  role_name = "tenant-${var.tenant_name}"

  # Allow any ServiceAccount in the tenant namespace to assume this role.
  # Narrow to specific SAs (e.g. ["app-sa"]) once workload identity is set up.
  bound_service_account_names      = ["*"]
  bound_service_account_namespaces = ["tenant-${var.tenant_name}"]

  token_policies = ["tenant-read-${var.tenant_name}"]
  token_ttl      = var.token_ttl
}
