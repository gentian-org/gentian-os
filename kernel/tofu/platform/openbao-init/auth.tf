# =============================================================================
# Kubernetes Auth Method
# =============================================================================
# Enables OpenBao to verify Kubernetes ServiceAccount tokens, so that
# External Secrets Operator can authenticate without a long-lived static token.
# Uses the hashicorp/vault provider (vault_* resources) which is API-compatible
# with OpenBao.

resource "vault_auth_backend" "kubernetes" {
  type        = "kubernetes"
  path        = "kubernetes"
  description = "Kubernetes service-account auth for ESO"
}

resource "vault_kubernetes_auth_backend_config" "default" {
  backend         = vault_auth_backend.kubernetes.path
  kubernetes_host = var.kubernetes_host
  # When OpenBao runs inside the same cluster it can discover the CA and
  # JWT issuer automatically from its own pod — no extra certs needed.
}

# One role per environment so that ESO can only read secrets for its own env.
resource "vault_kubernetes_auth_backend_role" "eso" {
  for_each = toset(var.environments)

  backend                          = vault_auth_backend.kubernetes.path
  role_name                        = "eso-${each.key}"
  bound_service_account_names      = [var.eso_service_account]
  bound_service_account_namespaces = [var.eso_namespace]
  token_policies                   = ["eso-read-${each.key}"]
  token_ttl                        = var.token_ttl
}

# Kubernetes auth role for the Tofu Controller runner pod.
# The runner pod uses the tf-runner ServiceAccount in tofu-system.
resource "vault_kubernetes_auth_backend_role" "tofu_runner" {
  backend                          = vault_auth_backend.kubernetes.path
  role_name                        = "tofu-runner"
  bound_service_account_names      = ["tf-runner"]
  bound_service_account_namespaces = ["tofu-system"]
  token_policies                   = ["tofu-write"]
  token_ttl                        = 3600
}
