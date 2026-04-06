# =============================================================================
# clients.tf — Keycloak OIDC clients for the opendesk realm
# =============================================================================

# ── OpenBao secret imports ────────────────────────────────────────────────────
# Each app module's vault_kv_secret_v2 resource writes the Keycloak client
# secret to an OpenBao path.  That path is SHARED — it also holds other keys
# (admin passwords, tokens, etc.) seeded by seed-openbao.sh.
#
# Importing the existing OpenBao secret into Terraform state + the module's
# lifecycle { ignore_changes = [data_json] } ensures that the first apply
# never overwrites the entire secret with only the oidc_client_secret key.

import {
  to = module.intercom.vault_kv_secret_v2.client_secret
  id = "secret/data/gentian-os/kernel/identity/keycloak-bootstrap"
}

import {
  to = module.nextcloud.vault_kv_secret_v2.client_secret
  id = "secret/data/gentian-os/kernel/apps/nextcloud"
}

import {
  to = module.ox_appsuite.vault_kv_secret_v2.client_secret
  id = "secret/data/gentian-os/kernel/apps/ox"
}

import {
  to = module.dovecot.vault_kv_secret_v2.client_secret
  id = "secret/data/gentian-os/kernel/mail/dovecot"
}

# ── opendesk-intercom ─────────────────────────────────────────────────────────
# No import — client does not exist in Keycloak yet; tofu creates it.
# client_secret is pinned to the seed-derived OpenBao value so the running ICS
# pod (which reads intercom.oidc_client_secret) stays consistent.

module "intercom" {
  source = "../../modules/app"

  realm_id     = var.realm
  client_id    = "opendesk-intercom"
  display_name = "opendesk Intercom"

  redirect_uris = ["https://ics.desk.gentian.org/callback"]
  web_origins   = ["+"]

  standard_flow_enabled = true

  backchannel_logout_url                   = "https://ics.desk.gentian.org/backchannel-logout"
  backchannel_logout_session_required      = true
  backchannel_logout_revoke_offline_tokens = true

  token_exchange_enabled = true

  extra_default_scopes = ["offline_access"]
  optional_scopes      = ["address", "phone", "microprofile-jwt", "organization"]

  client_secret = data.vault_kv_secret_v2.intercom_secrets.data["oidc_client_secret"]

  openbao_mount       = "secret"
  openbao_secret_path = "gentian-os/kernel/identity/keycloak-bootstrap"
  openbao_secret_key  = "intercom_client_secret"
}

# ── Intercom protocol mappers ─────────────────────────────────────────────────
# No imports — mappers are created fresh when the client is first created.

resource "keycloak_openid_user_attribute_protocol_mapper" "intercom_username" {
  realm_id  = var.realm
  client_id = module.intercom.client_uuid
  name      = "opendesk_username"

  user_attribute   = "uid"
  claim_name       = "opendesk_username"
  claim_value_type = "String"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

resource "keycloak_openid_user_attribute_protocol_mapper" "intercom_useruuid" {
  realm_id  = var.realm
  client_id = module.intercom.client_uuid
  name      = "opendesk_useruuid"

  user_attribute   = "entryUUID"
  claim_name       = "opendesk_useruuid"
  claim_value_type = "String"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ── opendesk-nextcloud ────────────────────────────────────────────────────────
# Client was created manually during first deployment. Now managed by tofu.
# UUID confirmed via: kcadm.sh get clients -r opendesk --fields id,clientId

import {
  to = module.nextcloud.keycloak_openid_client.this
  id = "${var.realm}/0b162651-3da3-4190-ad27-00303b3f51c7"
}

module "nextcloud" {
  source = "../../modules/app"

  realm_id     = var.realm
  client_id    = "opendesk-nextcloud"
  display_name = "Nextcloud"

  redirect_uris = ["https://files.desk.gentian.org/*", "https://portal.desk.gentian.org/*"]
  web_origins   = ["+"]

  backchannel_logout_url              = "https://files.desk.gentian.org/apps/user_oidc/backchannel-logout/opendesk"
  backchannel_logout_session_required = true

  post_logout_redirect_uris = ["https://files.desk.gentian.org/*", "https://portal.desk.gentian.org/*"] # maps to valid_post_logout_redirect_uris

  client_secret = data.vault_kv_secret_v2.nextcloud.data["oidc_client_secret"]

  openbao_mount       = "secret"
  openbao_secret_path = "gentian-os/kernel/apps/nextcloud"
  openbao_secret_key  = "oidc_client_secret"
}

# ── Nextcloud protocol mapper ─────────────────────────────────────────────────
# Maps entryUUID LDAP attribute to opendesk_useruuid claim.
# Nextcloud management chart uses mappingUid: "opendesk_useruuid".
# No import — mapper is created fresh by tofu on first apply.

resource "keycloak_openid_user_attribute_protocol_mapper" "nextcloud_useruuid" {
  realm_id  = var.realm
  client_id = module.nextcloud.client_uuid
  name      = "opendesk_useruuid"

  user_attribute   = "entryUUID"
  claim_name       = "opendesk_useruuid"
  claim_value_type = "String"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ── opendesk-oxappsuite ───────────────────────────────────────────────────────
# New client (no import — does not exist yet in Keycloak).
# Created as part of Phase 4D-core OX App Suite deployment.
#
# OX needs two protocol mappers:
#   1. opendesk_username  — maps uid LDAP attr  → opendesk_username claim
#      (used by com.openexchange.oidc.userLookupClaim)
#   2. context            — hardcoded "1"         → context claim
#      (used by com.openexchange.oidc.contextLookupClaim)

module "ox_appsuite" {
  source = "../../modules/app"

  realm_id     = var.realm
  client_id    = "opendesk-oxappsuite"
  display_name = "OX App Suite"

  redirect_uris = ["https://oxapps.desk.gentian.org/appsuite/api/oidc/auth"]
  web_origins   = ["+"]

  standard_flow_enabled = true

  backchannel_logout_url              = "https://oxapps.desk.gentian.org/appsuite/api/oidc/logout"
  backchannel_logout_session_required = true

  post_logout_redirect_uris = ["https://oxapps.desk.gentian.org/*", "https://portal.desk.gentian.org/*"] # maps to valid_post_logout_redirect_uris

  client_secret = data.vault_kv_secret_v2.ox.data["oidc_client_secret"]

  openbao_mount       = "secret"
  openbao_secret_path = "gentian-os/kernel/apps/ox"
  openbao_secret_key  = "oidc_client_secret"
}

# ── OX protocol mapper — opendesk_username ────────────────────────────────────

resource "keycloak_openid_user_attribute_protocol_mapper" "ox_username" {
  realm_id  = var.realm
  client_id = module.ox_appsuite.client_uuid
  name      = "opendesk_username"

  user_attribute   = "uid"
  claim_name       = "opendesk_username"
  claim_value_type = "String"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ── OX protocol mapper — context (hardcoded to "1") ───────────────────────────

resource "keycloak_openid_hardcoded_claim_protocol_mapper" "ox_context" {
  realm_id  = var.realm
  client_id = module.ox_appsuite.client_uuid
  name      = "context"

  claim_name       = "context"
  claim_value      = "1"
  claim_value_type = "String"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ── opendesk-dovecot ─────────────────────────────────────────────────────────
# New client (no import — created as part of Dovecot deployment).
# Dovecot uses token introspection to validate IMAP login tokens.
# No standard_flow — the client only introspects tokens, never redirects users.

module "dovecot" {
  source = "../../modules/app"

  realm_id     = var.realm
  client_id    = "opendesk-dovecot"
  display_name = "Dovecot IMAP"

  standard_flow_enabled = false
  redirect_uris         = []
  web_origins           = []

  client_secret = data.vault_kv_secret_v2.dovecot_secrets.data["oidc_client_secret"]

  openbao_mount       = "secret"
  openbao_secret_path = "gentian-os/kernel/mail/dovecot"
  openbao_secret_key  = "oidc_client_secret"
}

# ── Phase 4C stubs ────────────────────────────────────────────────────────────
# Uncomment when collabora deploys.

# module "collabora" {
#   source = "../../modules/app"
#
#   realm_id     = var.realm
#   client_id    = "collabora"
#   display_name = "Collabora Online"
#
#   redirect_uris = ["https://office.desk.gentian.org/*"]
#   web_origins   = ["+"]
#
#   openbao_mount       = "secret"
#   openbao_secret_path = "gentian-os/kernel/apps/collabora"
#   openbao_secret_key  = "oidc_client_secret"
# }
