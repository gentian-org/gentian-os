# =============================================================================
# trusts.tf — audience mappers enabling token exchange
# =============================================================================
# Each module.app-trust call adds a keycloak_openid_audience_protocol_mapper
# to the named client so downstream services can identify themselves as the
# intended audience of the access token.
# =============================================================================

# opendesk-intercom trusts itself as an audience.
# This lets the intercom back-end validate tokens issued for this client.
module "intercom_self_trust" {
  source = "../../modules/app-trust"

  realm_id                 = var.realm
  client_id                = module.intercom.client_uuid
  included_client_audience = "opendesk-intercom"

  add_to_id_token     = false
  add_to_access_token = true
}

# ── Phase 4C stubs ────────────────────────────────────────────────────────────
# Add cross-service trusts here when Nextcloud / Collabora are configured.
#
# Example: Nextcloud trusts opendesk-intercom (so intercom can token-exchange
# into a Nextcloud-scoped token):
#
# module "nextcloud_trusts_intercom" {
#   source = "../../modules/app-trust"
#
#   realm_id                 = var.realm
#   client_id                = module.nextcloud.client_uuid
#   included_client_audience = "opendesk-intercom"
# }
