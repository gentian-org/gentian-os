#!/usr/bin/env python3
# =============================================================================
# scripts/lib/carry-over-role-holders.py — the roles held by group or by a
# tenant's switch, rewritten as role holders on the Cluster claim.
#
# Called by _claim_carry_over_role_holders (bootstrap.sh) at step 0, once per
# cluster: while the claim still names a group for securityOfficer, auditor,
# serviceAdmin or sharedAppsAdmin under spec.platformRoles, or a tenant's
# manifest still says spec.perimeter.adminsApprove. Those are held by name now
# (spec.platformAccess.roleHolders); the schema has neither field.
#
#   check <claim> <tenants-dir>   exit 0 when there is something to carry over
#   apply <claim> <tenants-dir>   read the members from Keycloak and rewrite
#
# apply reads Keycloak at KC_BASE (…/auth) with the master realm's admin
# credential in KC_USER and KC_PASS. KC_BASE empty means the cluster has no
# Keycloak yet, so nobody is in any group: the fields are dropped and nobody
# is carried. It prints "info: …", "warn: …" and "edited: <path>" lines, and
# exits non-zero, changing nothing, when Keycloak does not answer.
#
# Who is carried over, each with reason "carried over at upgrade":
#   - the members of each of those kernel-realm groups, as people of the
#     platform tenant holding the role over the cluster;
#   - a member of a tenant's perimeter group who is an administrator of the
#     tenant, as its perimeter approver (scope tenant:<t>);
#   - every administrator of a tenant whose manifest said adminsApprove, the
#     same.
# Anybody else is reported, not dropped in silence: a perimeter group member
# who is no administrator of the tenant, and a member of the platform tenant's
# perimeter group, whose role would now reach every tenant.
# =============================================================================
import json
import os
import re
import sys
import urllib.parse
import urllib.request

RETIRED = ["securityOfficer", "auditor", "serviceAdmin", "sharedAppsAdmin"]
# The old schema's default for the one retired field that had one.
DEFAULTS = {"securityOfficer": "gentian:platform:security"}
REASON = "carried over at upgrade"
PLATFORM = "platform"


def read(path):
    with open(path) as f:
        return f.read().split("\n")


def write(path, lines):
    with open(path, "w") as f:
        f.write("\n".join(lines))


def block(lines, key, indent):
    """Index range of `key:` at indent and every deeper line after it."""
    pat = re.compile(r"^" + " " * indent + re.escape(key) + r":")
    for i, line in enumerate(lines):
        if pat.match(line):
            j = i + 1
            while j < len(lines) and (lines[j].strip() == "" or len(lines[j]) - len(lines[j].lstrip()) > indent):
                j += 1
            while j > i + 1 and lines[j - 1].strip() == "":
                j -= 1
            return i, j
    return None


def scalar(v):
    v = v.strip()
    if v.startswith('"'):
        return json.loads(v)
    if v.startswith("'"):
        return v[1:-1].replace("''", "'")
    return v


def retired_groups(claim):
    """The retired fields of spec.platformRoles, each with its group."""
    found = {}
    span = block(claim, "platformRoles", 2)
    if span:
        for line in claim[span[0] + 1:span[1]]:
            m = re.match(r"^    (\w+):\s*(.*?)\s*$", line)
            if m and m.group(1) in RETIRED:
                found[m.group(1)] = scalar(m.group(2)) if m.group(2) else ""
    return found


def tenants(tenants_dir):
    """name -> (manifest path, lines, realm, adminsApprove)."""
    out = {}
    if not os.path.isdir(tenants_dir):
        return out
    for name in sorted(os.listdir(tenants_dir)):
        path = os.path.join(tenants_dir, name, "tenant.yaml")
        if not os.path.isfile(path):
            continue
        lines = read(path)
        realm = name
        approve = False
        for line in lines:
            m = re.match(r"^\s+keycloakRealm:\s*(\S+)", line)
            if m:
                realm = scalar(m.group(1))
            if re.match(r"^\s+adminsApprove:\s*true\b", line):
                approve = True
        has_block = block(lines, "perimeter", 2) is not None
        out[name] = (path, lines, realm, approve, has_block)
    return out


def holders(claim):
    """The entries of spec.platformAccess.roleHolders, as dicts."""
    span = block(claim, "platformAccess", 2)
    if not span:
        return []
    entries = []
    for line in claim[span[0] + 1:span[1]]:
        m = re.match(r"^\s*(- )?(\w+):\s*(.*?)\s*$", line)
        if not m or m.group(2) == "roleHolders":
            continue
        if line.lstrip().startswith("- "):
            entries.append({})
        if entries:
            entries[-1][m.group(2)] = scalar(m.group(3))
    return entries


def render(entries):
    out = ["  platformAccess:", "    roleHolders:"]
    if not entries:
        return ["  platformAccess:", "    roleHolders: []"]
    for e in entries:
        first = True
        for k in ["role", "scope", "tenant", "subject", "username", "email", "reason", "expiresAt"]:
            if not e.get(k):
                continue
            v = e[k]
            if not re.match(r"^[A-Za-z0-9._-]+$", v):
                v = json.dumps(v)
            out.append(("      - " if first else "        ") + k + ": " + v)
            first = False
    return out


class Keycloak:
    def __init__(self, base, user, password):
        self.base, self.user, self.password = base.rstrip("/"), user, password

    def token(self):
        body = urllib.parse.urlencode({"client_id": "admin-cli", "grant_type": "password",
                                       "username": self.user, "password": self.password}).encode()
        with urllib.request.urlopen(self.base + "/realms/master/protocol/openid-connect/token", body, timeout=15) as r:
            return json.load(r)["access_token"]

    def get(self, path):
        # A token per call: the master realm's live for 60 seconds.
        req = urllib.request.Request(self.base + path, headers={"Authorization": "Bearer " + self.token()})
        with urllib.request.urlopen(req, timeout=15) as r:
            return json.load(r)

    def members(self, realm, group):
        """The members of a top-level group, [] when the realm or group is not there."""
        q = urllib.parse.quote
        try:
            found = self.get(f"/admin/realms/{q(realm)}/groups?search={q(group)}&exact=true")
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return []
            raise
        ids = [g["id"] for g in found if g.get("name") == group]
        if not ids:
            return []
        people = self.get(f"/admin/realms/{q(realm)}/groups/{q(ids[0])}/members?max=10000&briefRepresentation=true")
        return [{"subject": p["id"], "username": p.get("username", ""), "email": p.get("email", "")} for p in people]


def main():
    mode, claim_path, tenants_dir = sys.argv[1:4]
    claim = read(claim_path)
    groups = retired_groups(claim)
    known = tenants(tenants_dir)
    switched = [t for t, v in known.items() if v[3] or v[4]]
    if mode == "check":
        sys.exit(0 if groups or switched else 1)

    base = os.environ.get("KC_BASE", "")
    kc = Keycloak(base, os.environ.get("KC_USER", ""), os.environ.get("KC_PASS", "")) if base else None
    have = holders(claim)
    keys = {(e.get("role"), e.get("tenant"), e.get("subject")) for e in have}
    added = []

    def add(role, scope, tenant, person):
        k = (role, tenant, person["subject"])
        if k in keys:
            return
        keys.add(k)
        added.append({"role": role, "scope": scope, "tenant": tenant, "reason": REASON, **person})
        print(f"info: {person['username'] or person['subject']} of tenant {tenant} holds {role} over {scope}")

    try:
        if kc:
            kernel = known[PLATFORM][2] if PLATFORM in known else os.environ.get("KERNEL_REALM", "kernel")
            for role in RETIRED:
                group = groups[role] if role in groups else DEFAULTS.get(role, "")
                if not group:
                    continue
                for p in kc.members(kernel, group):
                    add(role, "cluster", PLATFORM, p)
            for name, (_, _, realm, approve, _) in known.items():
                perimeter = kc.members(realm, f"gentian:tenant:{name}:perimeter")
                if name == PLATFORM or realm != name:
                    for p in perimeter:
                        print(f"warn: {p['username'] or p['subject']} approved public addresses of tenant {name} "
                              f"(gentian:tenant:{name}:perimeter) and is not carried over: "
                              + ("a perimeter approver of the platform tenant approves for every tenant; name one deliberately"
                                 if name == PLATFORM else "the tenant has no realm of its own"))
                    continue
                admins = kc.members(realm, f"gentian:tenant:{name}:admins")
                ids = {p["subject"] for p in admins}
                if approve:
                    for p in admins:
                        add("perimeterApprover", f"tenant:{name}", name, p)
                for p in perimeter:
                    if p["subject"] in ids:
                        add("perimeterApprover", f"tenant:{name}", name, p)
                    else:
                        print(f"warn: {p['username'] or p['subject']} approved public addresses of tenant {name} "
                              f"(gentian:tenant:{name}:perimeter) and is not carried over: a perimeter approver "
                              f"of a tenant is one of its administrators")
    except (urllib.error.URLError, OSError, KeyError, ValueError) as e:
        print(f"warn: Keycloak did not answer ({e}); nothing was carried over")
        sys.exit(2)

    # The claim: the retired fields out, the holders in.
    out = []
    span = block(claim, "platformRoles", 2)
    for i, line in enumerate(claim):
        if span and span[0] < i < span[1] and re.match(r"^    (" + "|".join(RETIRED) + r"):", line):
            continue
        if line.strip() in ("# The security officers approve what escapes the default posture: a group",
                            "# of their own, never the administrators', made empty by the install."):
            continue
        out.append(line)
    claim = out
    if added or have:
        entries = have + added
        span = block(claim, "platformAccess", 2)
        if span:
            claim = claim[:span[0]] + render(entries) + claim[span[1]:]
        else:
            at = block(claim, "platformRoles", 2)
            at = at[1] if at else len(claim)
            claim = claim[:at] + render(entries) + claim[at:]
    write(claim_path, claim)
    print(f"edited: {claim_path}")

    # The tenants: the switch out, with the comment above it.
    for name, (path, lines, _, _, has_block) in known.items():
        span = block(lines, "perimeter", 2)
        if not span:
            continue
        start = span[0]
        while start > 0 and lines[start - 1].startswith("  #"):
            start -= 1
        write(path, lines[:start] + lines[span[1]:])
        print(f"edited: {path}")


if __name__ == "__main__":
    main()
