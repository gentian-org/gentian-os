# =============================================================================
# users.tf — UDM user provisioning
# =============================================================================
# Manages human operator and service users in the Nubus LDAP directory.
#
# Key points:
#   - All users go under cn=users,${var.ldap_base}.
#   - Required minimum fields: username, firstname, lastname,
#     mailPrimaryAddress, password.
#   - lifecycle { ignore_changes = [data] } is intentional — UDM auto-sets
#     ~150 attributes (uidNumber, gidNumber, ox*, samba*, opendesk*) that
#     differ between the POST body and the GET response.  Terraform manages
#     the user lifecycle (create on add, delete on remove) but does NOT
#     auto-reconcile property drift.  To force a property update:
#       terraform taint udm_user.<name>
#   - Passwords come from OpenBao via vault_kv_secret_v2 data sources.
#     Never set them as literals in this file.
#
# Provisioning new users
# ──────────────────────
# 1. Add the password to OpenBao at the appropriate path (or derive it).
# 2. Add a vault_kv_secret_v2 data source for that path (or extend an
#    existing one).
# 3. Copy the udm_user.example block below, uncomment it, and fill in the
#    real values.
# 4. Run: kubectl annotate terraform udm-config-dev -n tofu-system \
#           reconcile.fluxcd.io/requestedAt="$(date +%s)"
#
# Importing pre-existing users
# ─────────────────────────────
# If a user already exists in UDM (e.g. created manually or by Nubus
# bootstrap), import it before adding the resource block:
#   terraform import udm_user.<name> uid=<username>,cn=users,${var.ldap_base}
# =============================================================================

# ── Example ───────────────────────────────────────────────────────────────────
# Uncomment and adapt to provision a new user.
#
# resource "restapi_object" "user_example" {
#   path = "/univention/udm/users/user"
#
#   data = jsonencode({
#     position = "cn=users,${var.ldap_base}"
#     properties = {
#       username           = "admin.example"
#       firstname          = "Example"
#       lastname           = "Admin"
#       mailPrimaryAddress = "admin.example@${var.domain}"
#       password           = data.vault_kv_secret_v2.nubus.data["admin_password"]
#
#       # opendesk app access flags (all default false, set to true to enable)
#       opendeskFileshareEnabled            = true
#       opendeskProjectmanagementEnabled    = false
#       opendeskKnowledgemanagementEnabled  = false
#       opendeskLivecollaborationEnabled    = false
#       opendeskVideoconferenceEnabled      = false
#     }
#   })
#
#   id_attribute  = "dn"
#   read_path     = "/univention/udm/users/user/{id}"
#   update_path   = "/univention/udm/users/user/{id}"
#   destroy_path  = "/univention/udm/users/user/{id}"
#   update_method = "PUT"
#
#   # See notes at top of file — UDM auto-sets many fields on creation.
#   lifecycle {
#     ignore_changes = [data]
#   }
# }
