variable "realm_id" {
  description = "Keycloak realm ID (name) in which to create the OIDC client."
  type        = string
}

variable "client_id" {
  description = "Keycloak client_id string (not the UUID). E.g. 'opendesk-intercom'."
  type        = string
}

variable "display_name" {
  description = "Human-readable name shown in the Keycloak admin UI. Defaults to client_id."
  type        = string
  default     = ""
}

variable "redirect_uris" {
  description = "Allowed redirect URIs for the OIDC authorisation code flow."
  type        = list(string)
}

variable "web_origins" {
  description = "Allowed CORS origins. '+' means 'same as redirect URIs'."
  type        = list(string)
  default     = ["+"]
}

variable "standard_flow_enabled" {
  description = "Enable the authorisation code (standard) flow."
  type        = bool
  default     = true
}

variable "backchannel_logout_url" {
  description = "URL for OIDC back-channel logout notifications. Leave empty to skip."
  type        = string
  default     = ""
}

variable "backchannel_logout_session_required" {
  description = "Whether to require a session ID cookie in back-channel logout requests."
  type        = bool
  default     = true
}

# backchannel_logout_revoke_offline_tokens removed — mrparkers/keycloak v4.5.0 does not
# expose this field on keycloak_openid_client; it is configured in Keycloak via extra_config.
# Placeholder variable retained so callers that pass the argument don't break.
variable "backchannel_logout_revoke_offline_tokens" {
  description = "Revoke offline tokens on back-channel logout (unused — not supported by provider v4.5)."
  type        = bool
  default     = true
}

variable "client_secret" {
  description = "Set a specific Keycloak client secret instead of using the auto-generated one. Should match the seed-derived value stored in OpenBao so services remain consistent after re-deployments."
  type        = string
  default     = ""
  sensitive   = true
}

variable "token_exchange_enabled" {
  description = "Enable standard (RFC 8693) token exchange for this client."
  type        = bool
  default     = false
}

variable "extra_default_scopes" {
  description = "Additional default client scopes to assign (beyond the realm defaults)."
  type        = list(string)
  default     = []
}

variable "optional_scopes" {
  description = "Explicit optional scope list for the client. When set, fully replaces the inherited optional scopes. Use this when moving an optional scope (e.g. offline_access) to default scopes, so Keycloak doesn't reject the conflict."
  type        = list(string)
  default     = null
}

variable "post_logout_redirect_uris" {
  description = "Allowed post-logout redirect URIs. Keycloak uses these to validate where the browser is sent after RP-initiated logout."
  type        = list(string)
  default     = []
}

variable "openbao_mount" {
  description = "OpenBao / Vault KV v2 mount path."
  type        = string
  default     = "secret"
}

variable "openbao_secret_path" {
  description = "KV v2 path (sans mount) where the client_secret is stored. E.g. 'gentian-os/kernel/identity/keycloak-bootstrap'."  type        = string
}

variable "openbao_secret_key" {
  description = "Key within the KV secret that holds the client_secret. E.g. 'intercom_client_secret'."
  type        = string
}
