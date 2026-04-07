terraform {
  required_providers {
    keycloak = {
      source  = "mrparkers/keycloak"
      version = "~> 4.0"
    }
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
  }
}

# ── Keycloak OIDC client ───────────────────────────────────────────────────────

resource "keycloak_openid_client" "this" {
  realm_id  = var.realm_id
  client_id = var.client_id
  name      = var.display_name != "" ? var.display_name : var.client_id

  access_type                  = "CONFIDENTIAL"
  standard_flow_enabled        = var.standard_flow_enabled
  direct_access_grants_enabled = false
  service_accounts_enabled     = false

  client_secret = var.client_secret != "" ? var.client_secret : null

  valid_redirect_uris = var.redirect_uris
  web_origins         = var.web_origins

  backchannel_logout_url              = var.backchannel_logout_url != "" ? var.backchannel_logout_url : null
  backchannel_logout_session_required = var.backchannel_logout_session_required
  # backchannel_logout_revoke_offline_tokens — not supported in mrparkers/keycloak v4.x

  valid_post_logout_redirect_uris = length(var.post_logout_redirect_uris) > 0 ? var.post_logout_redirect_uris : null

  extra_config = merge(
    var.token_exchange_enabled ? {
      "standard.token.exchange.enabled"                         = "true"
      "standard.token.exchange.enableRefreshRequestedTokenType" = "SAME_SESSION"
    } : {},
  )
}

# ── Optional extra default scopes ─────────────────────────────────────────────
# Keycloak provider manages default_scopes as a separate resource when the list
# is non-empty so we can add scopes without clobbering realm defaults.

resource "keycloak_openid_client_default_scopes" "extra" {
  count     = length(var.extra_default_scopes) > 0 ? 1 : 0
  realm_id  = var.realm_id
  client_id = keycloak_openid_client.this.id

  default_scopes = concat(
    ["profile", "email", "roles", "web-origins"],
    var.extra_default_scopes,
  )
}

# ── Optional scope override ───────────────────────────────────────────────────
# When a scope is moved from optional to default (via extra_default_scopes),
# Keycloak rejects it if it still appears in the optional list.
# Set optional_scopes explicitly to resolve the conflict in one apply.

resource "keycloak_openid_client_optional_scopes" "override" {
  count     = var.optional_scopes != null ? 1 : 0
  realm_id  = var.realm_id
  client_id = keycloak_openid_client.this.id

  optional_scopes = var.optional_scopes
}

# ── Store the generated client_secret in OpenBao ──────────────────────────────
# lifecycle.ignore_changes ensures that once written the secret is never
# overwritten by subsequent applies (e.g. if Keycloak rotates the value).

resource "vault_kv_secret_v2" "client_secret" {
  mount     = var.openbao_mount
  name      = var.openbao_secret_path
  data_json = jsonencode({ (var.openbao_secret_key) = keycloak_openid_client.this.client_secret })
  lifecycle { ignore_changes = [data_json] }
}
