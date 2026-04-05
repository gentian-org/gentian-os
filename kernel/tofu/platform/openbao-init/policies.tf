# =============================================================================
# Policies
# =============================================================================
# Each environment gets a read-only policy scoped to its own secret path.
# ESO requires only `read` (via the `data` endpoint of KV v2).

resource "vault_policy" "eso_read" {
  for_each = toset(var.environments)

  name = "eso-read-${each.key}"
  policy = <<-EOT
    # Read-only access to all secrets under gentian/<env>/
    path "secret/data/gentian/${each.key}/*" {
      capabilities = ["read"]
    }

    # Allow listing secret keys (needed by some ESO features, not always required)
    path "secret/metadata/gentian/${each.key}/*" {
      capabilities = ["list"]
    }
  EOT
}

# Tofu runner needs read+write access to seed and read secrets used by helm
# releases managed by the infra-workspaces Terraform workspace.
resource "vault_policy" "tofu_write" {
  name = "tofu-write"
  policy = <<-EOT
    # Read/write access for Tofu runner to manage helm-release secrets
    path "secret/data/gentian/*" {
      capabilities = ["create", "read", "update", "delete"]
    }

    path "secret/metadata/gentian/*" {
      capabilities = ["list", "read", "delete"]
    }

    # The vault provider creates a limited child token for each operation.
    # This requires auth/token/create capability on the token itself.
    path "auth/token/create" {
      capabilities = ["update"]
    }

    # Allow the runner to look up its own token (required by vault provider v4).
    path "auth/token/lookup-self" {
      capabilities = ["read"]
    }
  EOT
}
