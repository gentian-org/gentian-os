#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2025 Gentian GmbH
#
# Patch slapd.conf to give tenant admins the LDAP access needed to provision users.
#
# Six insertions are made (all idempotent):
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
expected_patches = 8  # 3× cn=temporary + 2× tenant-OU + 1× self-write
                       # + 1× userPassword + 1× sambaAcctFlags + 1× shadowMax

if already_done >= 8:
    print("slapd.conf already fully patched for Tenant Admins, skipping.")
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

# Patch 2a: OU entry itself — children/entry access so Tenant Admins can add users
ou_entry_rule = f'access to dn.regex="^ou=[^,]+,{re.escape(ldap_base)}$" attrs=children,entry\n'
if "ou_entry_rule" not in content and ou_entry_rule not in content:
    new_rules += (
        f'# Gentian: tenant admins may add/remove entries inside their OU\n'
        f'access to dn.regex="^ou=[^,]+,{ldap_base}$" attrs=children,entry\n'
        f'   by {tenant_admins_set}\n'
        f'   by * +0 break\n'
    )

# Patch 2b: anything under any OU — full write for Tenant Admins
ou_children_rule = f'dn.regex="^.+,ou=[^,]+,{re.escape(ldap_base)}$"'
if ou_children_rule not in content:
    new_rules += (
        f'# Gentian: tenant admins may write objects under any tenant OU\n'
        f'access to dn.regex="^.+,ou=[^,]+,{ldap_base}$"\n'
        f'   by {tenant_admins_set}\n'
        f'   by * +0 break\n'
    )

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

with open(SLAPD_CONF, "w") as f:
    f.write(content)

print(
    f"Patched slapd.conf: {patched_count} cn=temporary block(s); "
    f"tenant-OU write rules; univentionUMCProperty self-write."
)
