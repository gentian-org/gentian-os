# =============================================================================
# KV v2 Secret Engine
# =============================================================================
# One KV v2 mount at `secret/` — this is the standard path.
# All gentian secrets live under secret/gentian/<env>/<component>.
#
# Actual secret *values* are NOT managed here to avoid storing them in
# Terraform state files.  Use scripts/seed-openbao.sh to write/rotate them.

resource "vault_mount" "kv" {
  path        = "secret"
  type        = "kv"
  description = "KV v2 store for all Gentian secrets"

  options = {
    version = "2"
  }
}
