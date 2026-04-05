# =============================================================================
# OpenBao data sources — nubus only (infra-workspaces workspace)
# =============================================================================
# PostgreSQL and MariaDB are currently managed by ArgoCD (legacy StatefulSets
# in gentian-infra-{env}) while the Tofu helm_release resources are at count=0
# pending data migration.  Their vault data sources live in stubs.tf.
# Only nubus uses a vault_kv_secret_v2 here.
# =============================================================================

data "vault_kv_secret_v2" "nubus" {
  mount = "secret"
  name  = "gentian/${var.env}/nubus"
}

data "vault_kv_secret_v2" "postfix" {
  mount = "secret"
  name  = "gentian/${var.env}/postfix"
}
