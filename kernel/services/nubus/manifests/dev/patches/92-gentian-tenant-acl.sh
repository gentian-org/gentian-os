#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2025 Gentian GmbH
#
# Patch slapd.conf to give tenant admins the LDAP access needed to provision users.
#
# Eleven insertions are made (all idempotent):
#
# 1. cn=temporary ACL — insert 'by set=Tenant Admins' before Domain Admins in the
#    three lock-object blocks, so tenant admins can acquire UID/SID locks when
#    creating users.
#
# 2. Tenant-OU write — insert two rules before 'access to *' that grant
#    cn=Tenant Admins write access to:
#    a) The OU entry itself (children/entry), so they can add objects to the OU.
#    b) Any entry under any OU (regex '^.+,ou='), so they can create/modify users.
#    This deliberately excludes cn=users, cn=groups, cn=policies, etc.
#
# 3. univentionUMCProperty self-write — insert a rule before 'access to *' that
#    allows users to write their own univentionUMCProperty (and objectClass if
#    needed to add the auxiliary univentionPerson class). Without this, UMC cannot
#    persist the user's last-used container, causing the wizard to reset to
#    cn=users on every login instead of remembering ou=<tenant>.
#
# 4. userPassword / krb5 / samba credential attributes — these have an explicit
#    'by * none' which stops evaluation and denies tenant admins completely.
#    Insert 'by set=Tenant Admins write' before 'by * none' so tenant admins can
#    set passwords when creating users.
#
# 5. sambaAcctFlags — 'by * +0 break' falls through to the read-only catchall.
#    Insert 'by set=Tenant Admins write' so tenant admins can set account flags.
#
# 6. shadowMax / krb5PasswordEnd / shadowLastChange — same break-through pattern.
#    Insert 'by set=Tenant Admins write' so tenant admins can set expiry attrs.
#
# 7. managed-by-attribute group membership — openDesk attribute-auto groups
#    (cn=managed-by-attribute-*,cn=groups,...): Tenant Admins write on
#    memberUid/uniqueMember so the post-create hook can add users.
#
# 8. cn=Domain Users group membership — UDM adds every new user to this
#    standard global group. Tenant Admins need write on memberUid/uniqueMember.
#
# 9. Tenant OU read restriction — the slapd.conf catch-all 'by users read' grants
#    any authenticated user read access to the entire LDAP tree, including other
#    tenants' ou= subtrees (user records, hashed passwords, email addresses, etc.).
#    Two rules inserted just before the catch-all restrict reads on tenant OU
#    entries and their contents to same-tenant users and service/admin accounts.
#    Uses OpenLDAP's '$1' back-reference: the tenant OU name captured from the
#    'access to dn.regex' pattern is expanded into the 'by dn.regex' subject, so
#    a single rule pair covers all tenant OUs without per-tenant configuration.
#    All matching 'by' clauses use 'read break' so that write access granted by
#    the earlier Tenant-OU write rules (patch 2) still accumulates for admins.
#
# 10. mail/domain visibility restriction — the same catch-all makes all
#     mail/domain objects (cn=<tenant>.<base>,cn=domain,cn=mail,...) visible to
#     every authenticated user.  This causes the domain dropdown in the UMC user
#     creation wizard to show all tenant domains and the parent domain, letting a
#     tenant admin select a domain that does not belong to their tenant (user
#     provisioning then fails because the email address is invalid for their OU).
#     A single rule inserted before the catch-all restricts each mail/domain read
#     to users in the matching tenant OU: the first DNS label of the domain CN
#     (e.g. 'gtn-test' from 'gtn-test.gentian.cloud') is captured as '$1' and
#     matched against 'ou=$1' in the requesting user's DN.  System and admin
#     accounts always get read access via 'break' clauses before the tenant check.
#
# 11. global usertemplate visibility — kernel templates at
#     cn=<name>,cn=templates,cn=univention,... are readable via the catch-all
#     'by users read', so tenant admins see the kernel "App User" template in
#     the UMC picker and UCR default points at the wrong mail domain.  Restrict
#     reads to admin/service accounts; tenant admins only see templates under
#     cn=templates,ou=<tenant>,... in their own OU (patch 9 write/read rules).
#
# Runs as /entrypoint.d/92-gentian-tenant-acl.sh before slapd starts.
# Idempotent: exits 0 without changes if all patches are already applied.
import re
import sys

SLAPD_CONF = "/etc/ldap/slapd.conf"
UCR_BASE_CONF = "/etc/univention/base.conf"

# Read ldap/base from UCR configuration
ldap_base = None
try:
    with open(UCR_BASE_CONF) as f:
        for line in f:
            if line.startswith("ldap/base:"):
                ldap_base = line.split(":", 1)[1].strip()
                break
except FileNotFoundError:
    print(f"ERROR: {UCR_BASE_CONF} not found", file=sys.stderr)
    sys.exit(1)

if not ldap_base:
    print("ERROR: ldap/base not found in UCR config", file=sys.stderr)
    sys.exit(1)

tenant_admins_group = f"cn=Tenant Admins,cn=groups,{ldap_base}"
tenant_admins_set   = f'set="user & [{tenant_admins_group}]/uniqueMember*" write'
domain_admins_by    = f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
tenant_admins_by    = f'   by set="user & [{tenant_admins_group}]/uniqueMember*" write\n'

with open(SLAPD_CONF) as f:
    content = f.read()

already_done = content.count("Tenant Admins")
# 3× cn=temporary + 1× userPassword + 1× sambaAcctFlags + 1× shadowMax
# + 1× managed-by-attribute group membership
# + 1× cn=Domain Users group membership = 8
# (Patches 2a and 2b no longer reference Tenant Admins — they use dn.regex
#  back-references to restrict write/read to the same-tenant OU only. The old
#  'by set=Tenant Admins write' granted cross-tenant read via write-implies-read,
#  reaching patch 9's deny rule too late. The dn.regex approach ensures patch 9
#  is reached for cross-tenant access so it can deny it.)
patch9_sentinel = f'# Gentian patch 9a: restrict read on tenant OU entries to same-tenant users'
# NOTE: using the comment as sentinel avoids a false-positive match against
# patch 2b's 'access to dn.regex="^.+,ou=([^,]+),..."' line (same pattern).
# Sentinel for the keycloak read grant added in patch 9b (upgrade detection).
# Old deployments have patch 9 but are missing this line, so the script must
# fall through and apply the in-place upgrade (elif branch below).
patch9_keycloak_sentinel = f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break'
# Sentinel for the nextcloud read grant added in patch 9 (upgrade detection).
# Deployments may have keycloak but not nextcloud; the elif branch below handles that.
patch9_nextcloud_sentinel = f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break'
# Sentinel for the dovecot read grant in patch 9 (upgrade detection).
# ldapsearch_dovecot is the bind DN used by Dovecot's LDAP passdb/userdb and
# must be able to read users in every tenant OU for IMAP authentication to work.
patch9_dovecot_sentinel = f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break'
# Sentinel for the svc-portal-server read grant in patch 9 (upgrade detection).
# svc-portal-server is the UDM REST API bind account used by the Nubus portal
# server's /api/v1/me endpoint.  Without this grant, UDM returns 404 when the
# portal server fetches a tenant admin user's data, causing a 500 error in the
# portal and preventing user-specific portal personalisation.
patch9_portal_sentinel = f'   by dn="uid=svc-portal-server,cn=users,{ldap_base}" read break'
# Sentinel for patch 10: mail/domain visibility restriction.
patch10_sentinel = f'# Gentian patch 10: restrict mail/domain visibility to owning tenant'
# Sentinel for the Tenant Admins read grant in patch 10 (upgrade detection).
# Old deployments have patch 10 but are missing this line; the elif branch
# below applies the in-place upgrade so tenant admins can resolve mail domains.
patch10_tenant_admins_sentinel = f'   by set="user & [cn=Tenant Admins,cn=groups,{ldap_base}]/uniqueMember*" read break\n   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
# Sentinel for the per-tenant OU write fix (patches 2a/2b v2).
# Old deployments have 'by set=Tenant Admins write' in these patches; new ones
# use 'by dn.regex="^.+,ou=$1,..." write' so only same-tenant users get write.
patch2_new_sentinel = f'   by dn.regex="^.+,ou=$1,{ldap_base}$" write\n'

if already_done >= 8 and patch9_sentinel in content and patch9_keycloak_sentinel in content and patch9_nextcloud_sentinel in content and patch9_dovecot_sentinel in content and patch9_portal_sentinel in content and patch2_new_sentinel in content and patch10_sentinel in content and patch10_tenant_admins_sentinel in content:
    print("slapd.conf already fully patched, skipping.")
    sys.exit(0)

# ── Patch 1: cn=temporary ACL ────────────────────────────────────────────────
blocks = re.split(r"(?=^access to )", content, flags=re.MULTILINE)
patched_count = 0
new_blocks = []
for block in blocks:
    if "cn=temporary" in block and domain_admins_by in block and tenant_admins_by not in block:
        block = block.replace(domain_admins_by, tenant_admins_by + domain_admins_by, 1)
        patched_count += 1
    new_blocks.append(block)
content = "".join(new_blocks)

if patched_count == 0 and already_done < 3:
    print("WARNING: No cn=temporary blocks with Domain Admins pattern found", file=sys.stderr)
    sys.exit(1)

# ── Patch 2 & 3: insert before the main 'access to *' catchall ────────────────
# The main catchall is: "access to *\n   by set=...Domain Admins.../uniqueMember* write\n   by users read\n"
catchall_marker = (
    f'access to *\n'
    f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
    f'   by users read\n'
)

if catchall_marker not in content:
    print("WARNING: main 'access to *' catchall not found in slapd.conf", file=sys.stderr)
    sys.exit(1)

new_rules = ""

# Patch 2a: OU entry itself — children/entry access so same-tenant admins can add users.
# Uses dn.regex back-reference so only users whose DN is UNDER THE SAME OU as the
# target entry get write. The old format used 'by set=Tenant Admins write' which
# granted cross-tenant write (and thus cross-tenant read, since write implies read),
# preventing patch 9's deny rule from being evaluated.
# NOTE: do NOT use re.escape(ldap_base) here. slapd.conf contains the raw base DN
# (e.g. dc=swp-ldap,dc=internal), not a regex-escaped version; the sentinel must
# match the string that was actually written to disk.
ou_entry_old = (
    f'# Gentian: tenant admins may add/remove entries inside their OU\n'
    f'access to dn.regex="^ou=[^,]+,{ldap_base}$" attrs=children,entry\n'
    f'   by {tenant_admins_set}\n'
    f'   by * +0 break\n'
)
ou_entry_new = (
    f'# Gentian: tenant admins may add/remove entries inside their own OU\n'
    f'access to dn.regex="^ou=([^,]+),{ldap_base}$" attrs=children,entry\n'
    f'   by dn.regex="^.+,ou=$1,{ldap_base}$" write\n'
    f'   by * +0 break\n'
)
if ou_entry_old in content:
    content = content.replace(ou_entry_old, ou_entry_new, 1)
    print("Upgraded patch 2a (OU entry) to per-tenant dn.regex scope.")
elif ou_entry_new not in content:
    new_rules += ou_entry_new

# Patch 2b: entries under any OU — write for same-tenant users only.
# CRITICAL: using 'by set=Tenant Admins write' here causes cross-tenant read
# exposure because write access implies read in OpenLDAP's access hierarchy.
# When admin-gtn-demo-2 (a Tenant Admin) reads objects in ou=gtn-demo, the set
# rule grants write (and thus read) BEFORE patch 9b's 'by * none' deny is reached.
# Replacing with a dn.regex back-reference means only same-OU users get write;
# cross-tenant users fall through to 'by * +0 break' and then to patch 9b's deny.
# (same ldap_base note as patch 2a: raw DN, not re.escape'd)
ou_children_old = (
    f'# Gentian: tenant admins may write objects under any tenant OU\n'
    f'access to dn.regex="^.+,ou=[^,]+,{ldap_base}$"\n'
    f'   by {tenant_admins_set}\n'
    f'   by * +0 break\n'
)
ou_children_new = (
    f'# Gentian: same-tenant users may write objects under their own tenant OU\n'
    f'access to dn.regex="^.+,ou=([^,]+),{ldap_base}$"\n'
    f'   by dn.regex="^.+,ou=$1,{ldap_base}$" write\n'
    f'   by * +0 break\n'
)
if ou_children_old in content:
    content = content.replace(ou_children_old, ou_children_new, 1)
    print("Upgraded patch 2b (OU children) to per-tenant dn.regex scope.")
elif ou_children_new not in content:
    new_rules += ou_children_new

# Patch 3: univentionUMCProperty self-write — UMC persists last-used container
if "univentionUMCProperty" not in content:
    new_rules += (
        f'# Gentian: users may update their own UMC preferences (last-used container, etc.)\n'
        f'access to attrs=univentionUMCProperty,objectClass\n'
        f'   by self write\n'
        f'   by * +0 break\n'
    )

if new_rules:
    content = content.replace(catchall_marker, new_rules + catchall_marker, 1)

# ── Patch 4: userPassword / krb5 / samba credential attrs ────────────────────
# 'by * none' hard-stops evaluation — tenant admins cannot set passwords.
# Unique context: the memberserver read line followed by 'by * none' and then
# the sambaAcctFlags rule is only present in this one block.
cred_context_old = (
    f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read\n'
    f'   by * none\n'
    f'access to attrs=sambaAcctFlags\n'
)
cred_context_new = (
    f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read\n'
    f'{tenant_admins_by}'
    f'   by * none\n'
    f'access to attrs=sambaAcctFlags\n'
)
if cred_context_old in content:
    content = content.replace(cred_context_old, cred_context_new, 1)
    print("Patched credential attrs (userPassword/krb5/samba) for Tenant Admins.")
elif cred_context_new in content:
    print("Credential attrs already patched.")
else:
    print("WARNING: credential attrs 'by * none' context not found in slapd.conf", file=sys.stderr)

# ── Patch 5: sambaAcctFlags ───────────────────────────────────────────────────
# 'by * +0 break' passes to read-only catchall. Unique context: samba block
# ends with dc write + break, immediately before shadowMax block.
samba_context_old = (
    f'access to attrs=sambaAcctFlags\n'
    f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
    f'   by dn.children="cn=dc,cn=computers,{ldap_base}" write\n'
    f'   by * +0 break\n'
    f'access to attrs=shadowMax,krb5PasswordEnd,shadowLastChange\n'
)
samba_context_new = (
    f'access to attrs=sambaAcctFlags\n'
    f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
    f'   by dn.children="cn=dc,cn=computers,{ldap_base}" write\n'
    f'{tenant_admins_by}'
    f'   by * +0 break\n'
    f'access to attrs=shadowMax,krb5PasswordEnd,shadowLastChange\n'
)
if samba_context_old in content:
    content = content.replace(samba_context_old, samba_context_new, 1)
    print("Patched sambaAcctFlags for Tenant Admins.")
elif samba_context_new in content:
    print("sambaAcctFlags already patched.")
else:
    print("WARNING: sambaAcctFlags context not found in slapd.conf", file=sys.stderr)

# ── Patch 6: shadowMax / krb5PasswordEnd / shadowLastChange ──────────────────
# Same break-through pattern. Unique context: memberserver read + break,
# immediately before cn=idmap block.
shadow_context_old = (
    f'access to attrs=shadowMax,krb5PasswordEnd,shadowLastChange\n'
    f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
    f'   by dn.children="cn=dc,cn=computers,{ldap_base}" write\n'
    f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read\n'
    f'   by * +0 break\n'
    f'access to dn.base="cn=idmap,cn=univention,{ldap_base}"'
)
shadow_context_new = (
    f'access to attrs=shadowMax,krb5PasswordEnd,shadowLastChange\n'
    f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
    f'   by dn.children="cn=dc,cn=computers,{ldap_base}" write\n'
    f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read\n'
    f'{tenant_admins_by}'
    f'   by * +0 break\n'
    f'access to dn.base="cn=idmap,cn=univention,{ldap_base}"'
)
if shadow_context_old in content:
    content = content.replace(shadow_context_old, shadow_context_new, 1)
    print("Patched shadowMax/krb5PasswordEnd/shadowLastChange for Tenant Admins.")
elif shadow_context_new in content:
    print("shadowMax block already patched.")
else:
    print("WARNING: shadowMax context not found in slapd.conf", file=sys.stderr)

# ── Patch 7: managed-by-attribute group membership ───────────────────────────
# UDM's post-create hook adds new users to openDesk attribute-managed groups
# (cn=managed-by-attribute-*,cn=groups,...). These are in cn=groups, not the
# tenant OU, so the tenant-OU write rule doesn't cover them. We grant write
# to memberUid/uniqueMember on this specific cn prefix only — deliberately
# excluding Domain Admins, Domain Users (handled by patch 8), and other privileged groups.
# (same ldap_base note as patch 2a: raw DN, not re.escape'd)
mab_rule = f'access to dn.regex="^cn=managed-by-attribute-[^,]+,cn=groups,{ldap_base}$" attrs=entry,memberUid,uniqueMember\n'
if mab_rule not in content:
    mab_acl = (
        f'# Gentian: tenant admins may manage openDesk attribute-auto groups\n'
        f'access to dn.regex="^cn=managed-by-attribute-[^,]+,cn=groups,{ldap_base}$" attrs=entry,memberUid,uniqueMember\n'
        f'   by {tenant_admins_set}\n'
        f'   by * +0 break\n'
    )
    content = content.replace(catchall_marker, mab_acl + catchall_marker, 1)
    print("Patched managed-by-attribute group membership for Tenant Admins.")
else:
    print("managed-by-attribute rule already present.")

# ── Patch 8: cn=Domain Users group membership ────────────────────────────────
# UDM adds every newly created user to cn=Domain Users (a standard global
# group). Tenant admins need write on memberUid/uniqueMember for this group.
domain_users_dn_rule = f'access to dn.base="cn=Domain Users,cn=groups,{ldap_base}" attrs=entry,memberUid,uniqueMember\n'
if domain_users_dn_rule not in content:
    domain_users_acl = (
        f'# Gentian: tenant admins may add users to cn=Domain Users\n'
        f'access to dn.base="cn=Domain Users,cn=groups,{ldap_base}" attrs=entry,memberUid,uniqueMember\n'
        f'   by {tenant_admins_set}\n'
        f'   by * +0 break\n'
    )
    content = content.replace(catchall_marker, domain_users_acl + catchall_marker, 1)
    print("Patched cn=Domain Users group membership for Tenant Admins.")
else:
    print("cn=Domain Users rule already present.")

# ── Patch 8b: cn=App Users group membership ──────────────────────────────────
# Gentian OS custom "App Users" global group. Tenant admins need write on
# memberUid/uniqueMember for this group.
app_users_dn_rule = f'access to dn.base="cn=App Users,cn=groups,{ldap_base}" attrs=entry,memberUid,uniqueMember\n'
if app_users_dn_rule not in content:
    app_users_acl = (
        f'# Gentian: tenant admins may add users to cn=App Users\n'
        f'access to dn.base="cn=App Users,cn=groups,{ldap_base}" attrs=entry,memberUid,uniqueMember\n'
        f'   by {tenant_admins_set}\n'
        f'   by * +0 break\n'
    )
    content = content.replace(catchall_marker, app_users_acl + catchall_marker, 1)
    print("Patched cn=App Users group membership for Tenant Admins.")
else:
    print("cn=App Users rule already present.")

# ── Patch 9: tenant OU read restriction ──────────────────────────────────────
# The catch-all 'by users read' grants every authenticated user read access to
# the whole LDAP tree.  Insert two rules immediately before the catch-all that
# restrict reads on tenant OU entries (9a) and their contents (9b) to:
#   • local socket / cn=admin / Domain Admins / DC & member-server computers
#   • uid=Administrator (UCS built-in, used by Keycloak LDAP federation)
#   • the requesting user's own entry (self)
#   • any user whose DN is under the SAME tenant OU as the target ($1 back-ref)
# All other authenticated users get 'none' — cross-tenant reads are denied.
# 'read break' is used on allowed clauses so write access from patch 2 rules
# still accumulates for tenant admins.
if patch9_sentinel not in content:
    # Fresh config: insert full rules (including keycloak service account) before catchall.
    # ldapsearch_keycloak is the bind DN used by Keycloak LDAP federation (see
    # kernel/services/nubus/values/_base.yaml) and must be able to read users in
    # every tenant OU regardless of the same-tenant restriction.
    ou_read_rules = (
        f'# Gentian patch 9a: restrict read on tenant OU entries to same-tenant users\n'
        f'access to dn.regex="^ou=([^,]+),{ldap_base}$"\n'
        f'   by sockname="PATH=/var/run/slapd/ldapi" read break\n'
        f'   by dn="cn=admin,{ldap_base}" read break\n'
        f'   by group/univentionGroup/uniqueMember="cn=Domain Admins,cn=groups,{ldap_base}" read break\n'
        f'   by dn.children="cn=dc,cn=computers,{ldap_base}" read break\n'
        f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read break\n'
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=svc-portal-server,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b: restrict read on entries inside tenant OUs to same-tenant users\n'
        f'access to dn.regex="^.+,ou=([^,]+),{ldap_base}$"\n'
        f'   by sockname="PATH=/var/run/slapd/ldapi" read break\n'
        f'   by dn="cn=admin,{ldap_base}" read break\n'
        f'   by group/univentionGroup/uniqueMember="cn=Domain Admins,cn=groups,{ldap_base}" read break\n'
        f'   by dn.children="cn=dc,cn=computers,{ldap_base}" read break\n'
        f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read break\n'
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=svc-portal-server,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
    )
    content = content.replace(catchall_marker, ou_read_rules + catchall_marker, 1)
    print("Patched tenant OU read restriction (patch 9).")
elif patch9_keycloak_sentinel not in content:
    # Upgrade path: patch 9 already applied (old version) but missing the
    # ldapsearch_keycloak grant.  Add the missing lines in-place rather than
    # re-inserting the whole block.
    old_9a_tail = (
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    new_9a_tail = (
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    old_9b_tail = (
        f'   by self read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'access to *\n'
        f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
        f'   by users read'
    )
    new_9b_tail = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'access to *\n'
        f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
        f'   by users read'
    )
    if old_9a_tail not in content or old_9b_tail not in content:
        print("WARNING: could not locate expected patch 9 tail for upgrade; skipping keycloak grant.",
              file=sys.stderr)
    else:
        content = content.replace(old_9a_tail, new_9a_tail, 1)
        content = content.replace(old_9b_tail, new_9b_tail, 1)
        print("Upgraded tenant OU read restriction to grant ldapsearch_keycloak access (patch 9 keycloak).")
elif patch9_nextcloud_sentinel not in content:
    # Upgrade path: patch 9 already has ldapsearch_keycloak but is missing the
    # ldapsearch_nextcloud grant. ldapsearch_nextcloud is the bind DN used by
    # Nextcloud's LDAP app and must be able to read users in every tenant OU
    # regardless of the same-tenant restriction (it lives in cn=users, not in
    # any tenant OU).
    old_9a_nextcloud = (
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    new_9a_nextcloud = (
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    old_9b_nextcloud = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    new_9b_nextcloud = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    if old_9a_nextcloud not in content or old_9b_nextcloud not in content:
        print("WARNING: could not locate expected patch 9 context for nextcloud upgrade; skipping.",
              file=sys.stderr)
    else:
        content = content.replace(old_9a_nextcloud, new_9a_nextcloud, 1)
        content = content.replace(old_9b_nextcloud, new_9b_nextcloud, 1)
        print("Upgraded tenant OU read restriction to grant ldapsearch_nextcloud access (patch 9 nextcloud).")
elif patch9_dovecot_sentinel not in content:
    # Upgrade path: patch 9 already has ldapsearch_keycloak and ldapsearch_nextcloud
    # but is missing ldapsearch_dovecot. Dovecot's LDAP passdb/userdb uses this
    # bind DN to look up users in tenant OUs for IMAP authentication.
    old_9a_dovecot = (
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    new_9a_dovecot = (
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    old_9b_dovecot = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    new_9b_dovecot = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    if old_9a_dovecot not in content or old_9b_dovecot not in content:
        print("WARNING: could not locate expected patch 9 context for dovecot upgrade; skipping.",
              file=sys.stderr)
    else:
        content = content.replace(old_9a_dovecot, new_9a_dovecot, 1)
        content = content.replace(old_9b_dovecot, new_9b_dovecot, 1)
        print("Upgraded tenant OU read restriction to grant ldapsearch_dovecot access (patch 9 dovecot).")
elif patch9_portal_sentinel not in content:
    # Upgrade path: patch 9 already has ldapsearch_dovecot but is missing
    # svc-portal-server. The Nubus portal server uses this UDM REST API bind
    # account to fetch user data in /api/v1/me. Without tenant-OU read access
    # the UDM call returns 404 and the portal server returns 500 for all tenant
    # admin users, breaking portal personalisation.
    old_9a_portal = (
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    new_9a_portal = (
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=svc-portal-server,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 9b'
    )
    old_9b_portal = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    new_9b_portal = (
        f'   by self read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_dovecot,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=svc-portal-server,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'# Gentian patch 10'
    )
    if old_9a_portal not in content or old_9b_portal not in content:
        print("WARNING: could not locate expected patch 9 context for svc-portal-server upgrade; skipping.",
              file=sys.stderr)
    else:
        content = content.replace(old_9a_portal, new_9a_portal, 1)
        content = content.replace(old_9b_portal, new_9b_portal, 1)
        print("Upgraded tenant OU read restriction to grant svc-portal-server access (patch 9 portal-server).")
else:
    print("Tenant OU read restriction (patch 9) with svc-portal-server already present.")

# ── Patch 10: mail/domain visibility restriction ──────────────────────────────
# The mail/domain objects live at cn=<domain>,cn=domain,cn=mail,<ldapbase> —
# outside any tenant OU.  The catch-all 'by users read' makes them visible to
# every authenticated user, so the UMC domain dropdown shows domains of other
# tenants and the parent domain.  Selecting any of those causes user creation
# to fail (the email address falls outside the admin's write scope).
#
# The fix uses an OpenLDAP $1 back-reference: the regex captures the first DNS
# label of the domain CN (everything up to the first dot, e.g. 'gtn-test' from
# 'gtn-test.gentian.cloud') and allows reads only from users whose DN contains
# 'ou=$1' — i.e. users in the matching tenant OU.  System and admin accounts
# always get access via 'read break' before the tenant-scope check.
#
# Mail domains that do NOT follow the '<tenant>.<rest>' naming pattern (e.g.
# a bare parent domain like 'gentian.cloud') are intentionally NOT matched by
# this rule (the dn.regex requires at least one dot in the CN portion), so they
# fall through to 'by * none' in this rule — making the parent domain invisible
# to tenant admins, which is the desired behaviour.
if patch10_sentinel not in content:
    mail_domain_acl = (
        f'# Gentian patch 10: restrict mail/domain visibility to owning tenant\n'
        f'access to dn.regex="^cn=([^,.]+)\\.[^,]+,cn=domain,cn=mail,{ldap_base}$"\n'
        f'   by sockname="PATH=/var/run/slapd/ldapi" read break\n'
        f'   by dn="cn=admin,{ldap_base}" read break\n'
        f'   by group/univentionGroup/uniqueMember="cn=Domain Admins,cn=groups,{ldap_base}" read break\n'
        f'   by dn.children="cn=dc,cn=computers,{ldap_base}" read break\n'
        f'   by dn.children="cn=memberserver,cn=computers,{ldap_base}" read break\n'
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_nextcloud,cn=users,{ldap_base}" read break\n'
        f'   by set="user & [cn=Tenant Admins,cn=groups,{ldap_base}]/uniqueMember*" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
    )
    content = content.replace(catchall_marker, mail_domain_acl + catchall_marker, 1)
    print("Patched mail/domain visibility restriction (patch 10).")
elif patch10_tenant_admins_sentinel not in content:
    # Upgrade: patch 10 is applied but missing Tenant Admins read grant.
    # Mail domains like 'gentian.org' have first-label 'gentian' which doesn't
    # match any tenant OU, so the existing dn.regex rule denies ALL tenant admins.
    # This breaks oxAccess.py mail domain validation when tenant admins create users.
    # Fix: add Tenant Admins read before the per-tenant dn.regex line.
    # Unique context: patch 10 has no 'by self read break' between Administrator and
    # ldapsearch_keycloak (unlike patch 9b), and patch 10 is the last rule before
    # the catchall — so this replacement uniquely targets patch 10.
    old_p10_tail = (
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'access to *\n'
        f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
        f'   by users read\n'
    )
    new_p10_tail = (
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by dn="uid=ldapsearch_keycloak,cn=users,{ldap_base}" read break\n'
        f'   by set="user & [cn=Tenant Admins,cn=groups,{ldap_base}]/uniqueMember*" read break\n'
        f'   by dn.regex="^.+,ou=$1,{ldap_base}$" read break\n'
        f'   by * none\n'
        f'access to *\n'
        f'   by set="user & [cn=Domain Admins,cn=groups,{ldap_base}]/uniqueMember*" write\n'
        f'   by users read\n'
    )
    if old_p10_tail not in content:
        print("WARNING: could not locate expected patch 10 tail for Tenant Admins upgrade; skipping.",
              file=sys.stderr)
    else:
        content = content.replace(old_p10_tail, new_p10_tail, 1)
        print("Upgraded mail/domain visibility to grant Tenant Admins read (patch 10 tenant-admins).")
else:
    print("mail/domain visibility restriction (patch 10) already present.")

# ── Patch 11: global usertemplate visibility restriction ─────────────────────
patch11_sentinel = f'# Gentian patch 11: restrict global usertemplate visibility\n'
if patch11_sentinel not in content:
    global_template_acl = (
        f'# Gentian patch 11: restrict global usertemplate visibility to admins\n'
        f'access to dn.regex="^cn=[^,]+,cn=templates,cn=univention,{ldap_base}$"\n'
        f'   by sockname="PATH=/var/run/slapd/ldapi" read break\n'
        f'   by dn="cn=admin,{ldap_base}" read break\n'
        f'   by group/univentionGroup/uniqueMember="cn=Domain Admins,cn=groups,{ldap_base}" read break\n'
        f'   by dn="uid=Administrator,cn=users,{ldap_base}" read break\n'
        f'   by * none\n'
    )
    content = content.replace(catchall_marker, global_template_acl + catchall_marker, 1)
    print("Patched global usertemplate visibility restriction (patch 11).")
else:
    print("global usertemplate visibility restriction (patch 11) already present.")

with open(SLAPD_CONF, "w") as f:
    f.write(content)

print(
    f"Patched slapd.conf: {patched_count} cn=temporary block(s); "
    f"tenant-OU write rules; univentionUMCProperty self-write; tenant OU read restriction; "
    f"mail/domain visibility restriction."
)
