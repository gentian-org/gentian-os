# =============================================================================
# groups.tf — UDM group membership management
# =============================================================================
# Nubus bootstraps a fixed set of groups (Domain Admins, Domain Users,
# managed-by-attribute-*, IAM API - Full Access, etc.).  This file manages
# their MEMBERSHIP — specifically, which users belong to each group.
#
# IMPORTANT: these groups already exist in LDAP.  DO NOT let Terraform
# attempt to recreate them.  Always import before adding a resource block:
#
#   export DN="cn=<Group Name>,cn=groups,${LDAP_BASE}"
#   terraform import restapi_object.<local_name> "$(python3 -c \
#     "import urllib.parse; print(urllib.parse.quote('${DN}', safe=''))")"
#
# After import the resource block keeps the group in sync.  Adding a user
# to `properties.users` on the next apply causes a PUT that updates members.
#
# Bootstrapped groups (from live cluster, 2026-05-08):
#   cn=Domain Admins,cn=groups,...
#   cn=Domain Users,cn=groups,...
#   cn=Domain Service Users,cn=groups,...
#   cn=2FA Users,cn=groups,...
#   cn=managed-by-attribute-Groupware,cn=groups,...
#   cn=managed-by-attribute-Fileshare,cn=groups,...
#   cn=managed-by-attribute-FileshareAdmin,cn=groups,...
#   cn=managed-by-attribute-Projectmanagement,cn=groups,...
#   cn=managed-by-attribute-ProjectmanagementAdmin,cn=groups,...
#   cn=IAM API - Full Access,cn=groups,...
#   cn=Tenant Admins,cn=groups,...
#
# Managing opendesk app access
# ─────────────────────────────
# opendesk uses attribute-controlled groups to gate feature access:
#   managed-by-attribute-Fileshare     → opendeskFileshareEnabled
#   managed-by-attribute-Groupware     → Groupware (OX) access
#   managed-by-attribute-Projectmanagement → opendeskProjectmanagementEnabled
# Add users to these groups to grant app access across Nextcloud, OX, etc.
#
# Provisioning new groups
# ────────────────────────
# Use the restapi_object pattern below with count=0 until ready:
#
#   resource "restapi_object" "group_example" {
#     path = "/univention/udm/groups/group"
#     data = jsonencode({
#       position = "cn=groups,${var.ldap_base}"
#       properties = {
#         name  = "gentian-operators"
#         users = []   # add user DNs here
#       }
#     })
#     id_attribute  = "dn"
#     read_path     = "/univention/udm/groups/group/{id}"
#     update_path   = "/univention/udm/groups/group/{id}"
#     destroy_path  = "/univention/udm/groups/group/{id}"
#     update_method = "PUT"
#   }
# =============================================================================

# No group resources are active by default.  Uncomment and import existing
# groups, or add new group definitions following the pattern above.
#
# Example: import Domain Admins and manage its membership.
#
# import {
#   to = restapi_object.group_domain_admins
#   id = "cn=Domain Admins,cn=groups,${var.ldap_base}"
# }
#
# resource "restapi_object" "group_domain_admins" {
#   path = "/univention/udm/groups/group"
#
#   data = jsonencode({
#     position = "cn=groups,${var.ldap_base}"
#     properties = {
#       name  = "Domain Admins"
#       users = [
#         "uid=Administrator,cn=users,${var.ldap_base}",
#         # "uid=admin.example,cn=users,${var.ldap_base}",
#       ]
#     }
#   })
#
#   id_attribute  = "dn"
#   read_path     = "/univention/udm/groups/group/{id}"
#   update_path   = "/univention/udm/groups/group/{id}"
#   destroy_path  = "/univention/udm/groups/group/{id}"
#   update_method = "PUT"
#
#   lifecycle {
#     # Prevent accidental deletion of system groups.
#     # To actually delete: remove prevent_destroy and set count = 0.
#     prevent_destroy = true
#   }
# }
