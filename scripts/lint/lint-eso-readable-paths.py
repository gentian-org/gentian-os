#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-eso-readable-paths.py — a credential nothing can read
# =============================================================================
# A secret in this platform has two halves. Something writes it to OpenBao, and
# External Secrets reads it back out into a Kubernetes Secret. The write is
# governed by the writer's policy; the read is governed by eso-read, which the
# Cluster composition renders. Both must cover the path, and they are declared
# in different files by different people at different times.
#
# When only the write half is covered, the failure is quiet and misleading. The
# value is stored. The path is correct. The credential simply never materialises:
#
#   error processing spec.data[0] (key: gentian-os/tenants/corp/backup/destination),
#   err: cannot read secret data from Vault: ... Code: 403 ... permission denied
#
# and the CredentialRequirement's satisfaction probe never syncs, so the console
# reports the credential as missing while it is sitting in the path. Whoever
# supplied it re-enters it, gets the same result, and starts auditing the wrong
# policy — the one that let them write it, which is correct.
#
# This has now happened three times, on the three tenant subtrees that are not
# `apps`:
#
#   tenants/<t>/repositories/<n>   Argo CD repository Secret never materialised
#   tenants/<t>/backup/destination BackupPolicy stuck CredentialUnsatisfied
#   tenants/<t>/contracts/<c>      found by this lint, before it was reached
#
# Each time the path was added to the code and eso-read was not extended. The
# pattern is not carelessness; it is that the two live nowhere near each other,
# and nothing connected them.
#
# WHAT IT CHECKS
#
# Every OpenBao path this repository constructs must be either
#
#   - readable by eso-read, or
#   - listed in OPERATOR_ONLY below, with a reason.
#
# Default-deny, so a new path has to be classified rather than defaulting into
# silence. The two entries there today are a purge subtree root and a derivation
# salt: neither is read back through a Secret, and both would be wrong to expose
# to ESO.
#
# The policy is read from the golden render rather than restated here, for the
# reason scripts/tools/verify-openbao-policies.sh gives: a test carrying its own
# copy of the thing under test asserts only that the copy is self-consistent.
#
# WHAT IT DOES NOT CHECK
#
# That the WRITER can write. eso-read is one half; the tenant-<t> and
# cluster-admin policies are the other, and which writer a path belongs to is
# not derivable from the path.
#
# Nor whether a readable path is one ESO SHOULD read. eso-read grants
# `kernel/*` wholesale, so every kernel path passes here whether or not it ever
# becomes a Secret. Narrowing that is a separate question from this one.
#
# Usage:
#   python3 scripts/lint/lint-eso-readable-paths.py
# =============================================================================
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
GOLDEN = ROOT / "crossplane/tests/unit/render/cluster-oidc-policies/expected.yaml"
GO_ROOTS = [ROOT / "internal", ROOT / "cmd"]
COMPOSITIONS = ROOT / "crossplane/compositions"
CREDENTIALS = ROOT / "credentials.yaml"

ESO_POLICY_NAME = "eso-read"
KV_DATA_PREFIX = "secret/data/"

RED = "\033[0;31m"
GREEN = "\033[0;32m"
YELLOW = "\033[0;33m"
DIM = "\033[2m"
NC = "\033[0m"

# A vault path this platform builds but ESO must not read. Each entry is the
# normalised pattern (see normalise) and why it is not a Secret.
OPERATOR_ONLY = {
    "gentian-os/tenants/+":
        "the tenant's whole subtree, addressed only by DeleteTree on cleanup "
        "(tenant_cleanup.go). Granting ESO read here would grant it every "
        "tenant secret at once, which is the opposite of what the named "
        "sub-paths are for.",
    "gentian-os/tenants/+/admin":
        "a derivation salt, not a stored credential (seeder.go). Nothing reads "
        "it back and no Secret is made from it.",
    "gentian-os/kernel/backup/identity":
        "the cluster's escrowed backup key, when spec.backup.escrowIdentity is "
        "on. Read by a cluster administrator performing a restore, and by "
        "--export-recovery-kit. Denied to ESO in the same policy: a key that "
        "could become a Secret would be readable by whoever takes the cluster, "
        "which is the one thing escrow must not come to mean.",
    "gentian-os/tenants/+/backup/identity":
        "one tenant's escrowed backup key (credentialmgr/backupidentity.go). "
        "Same reasoning one subtree down, and the reason the sibling grant on "
        "tenants/+/backup/* is not enough: the destination credential there "
        "exists to become a Secret, and this must never.",
}

# What a placeholder becomes when a pattern is made concrete. Any single
# segment does: a policy `+` matches one segment whatever it holds, so the
# sample value cannot change the verdict.
SAMPLE = "x"


def strip_go_comments(src: str) -> str:
    """Remove // and /* */ comments so documented example paths are not scanned.

    Several path helpers spell an example in their doc comment — openbao.go
    names "gentian-os/tenants/t/apps/a/oidc" — and a lint that reads those is
    checking prose. Crude by design: it can truncate a line at a URL's //, which
    costs a path only if one shared that line, and gains not reporting comments
    as findings.
    """
    src = re.sub(r"/\*.*?\*/", "", src, flags=re.S)
    return "\n".join(line.split("//", 1)[0] for line in src.splitlines())


def normalise(raw: str):
    """A repository path pattern as a concrete path, or None if not one.

    Format verbs and Go-template actions both stand for one path segment, so
    both become SAMPLE. A trailing slash means the code appends a segment.
    """
    path = raw.strip()
    # Go template actions, whole or partial: {{ $tenant }}, {{ printf ... }}
    path = re.sub(r"\{\{.*?\}\}", SAMPLE, path)
    if "{{" in path:                      # a fragment split across the match
        path = path.split("{{", 1)[0] + SAMPLE
    # printf verbs
    path = re.sub(r"%[#+\-0-9.]*[a-zA-Z]", SAMPLE, path)
    path = path.rstrip('"').strip()
    if path.endswith("/"):                # "prefix/" + name
        path += SAMPLE
    if path.endswith("."):                # sentence punctuation that crept in
        path = path[:-1].rstrip("/")
    if not path.startswith(("gentian-os/kernel/", "gentian-os/tenants/")):
        return None
    # A `+` or a trailing `*` means this is a policy glob, not a path something
    # builds — the composition's own policy text, matched by the same scan.
    if "+" in path or "*" in path:
        return None
    if path.rstrip("/") in ("gentian-os/kernel", "gentian-os/tenants"):
        return None
    return path.rstrip("/")


def policy_read_paths(text, yaml):
    """What eso-read can actually read: the `read` grants, minus the denies.

    Both halves, because the policy now has both. A path can sit inside a glob
    this policy reads and still be unreadable, which is how the escrowed backup
    identities work: `tenants/+/backup/*` is granted so destination credentials
    become Secrets, and `tenants/+/backup/identity` is denied inside it so the
    key that opens a tenant's whole history never can.

    Reading only the grants made this lint agree that those paths were readable
    when OpenBao refuses them — and, worse, it would have gone on agreeing if
    the deny were ever deleted. A lint whose subject is "can anything read this"
    has to model the half of the policy that says no.
    """
    granted, denied = [], []
    for doc in yaml.safe_load_all(text):
        if not isinstance(doc, dict):
            continue
        fp = ((doc.get("spec") or {}).get("forProvider") or {})
        if fp.get("name") != ESO_POLICY_NAME:
            continue
        body = fp.get("policy") or ""
        # Strip comments first: the policy text carries prose that names paths.
        body = "\n".join(l.split("#", 1)[0] for l in body.splitlines())
        for match in re.finditer(r'path\s+"([^"]+)"\s*\{([^}]*)\}', body):
            path, block = match.group(1), match.group(2)
            caps = re.search(r"capabilities\s*=\s*\[([^\]]*)\]", block)
            if not caps:
                continue
            if "deny" in caps.group(1):
                denied.append(path)
            elif "read" in caps.group(1):
                granted.append(path)
    return granted, denied


def vault_glob_matches(policy_path: str, concrete: str) -> bool:
    """Vault path-glob semantics: + is one segment, a trailing * is the rest.

    A `*` anywhere but the end is a literal in Vault, which is the detail that
    makes `oidc-+` match nothing and is worth honouring rather than treating
    every wildcard as a regex.
    """
    wild_tail = policy_path.endswith("*")
    body = policy_path[:-1] if wild_tail else policy_path
    pattern = "/".join(
        "[^/]+" if seg == "+" else re.escape(seg) for seg in body.split("/"))
    return re.fullmatch(pattern + (".*" if wild_tail else ""), concrete) is not None


def waiver_key(path: str) -> str:
    """OPERATOR_ONLY is written with `+` because that is how a path pattern
    reads; collected patterns carry SAMPLE in the same places. Compare in one
    vocabulary rather than asking the list to be written in the other."""
    return path.replace("+", SAMPLE)


def collect_patterns():
    """Every vault path this repository constructs, with where it came from."""
    found = {}

    def add(path, origin):
        norm = normalise(path)
        if norm:
            found.setdefault(norm, set()).add(origin)

    for root in GO_ROOTS:
        for go in sorted(root.rglob("*.go")):
            if go.name.endswith("_test.go"):
                continue
            src = strip_go_comments(go.read_text(errors="replace"))
            for lit in re.findall(r'"((?:[^"\\]|\\.)*)"', src):
                if "gentian-os/" in lit:
                    add(lit, str(go.relative_to(ROOT)))

    for comp in sorted(COMPOSITIONS.glob("*.yaml")):
        for line in comp.read_text(errors="replace").splitlines():
            if line.strip().startswith("#"):
                continue
            for hit in re.findall(r'gentian-os/[^"\'\s]*', line):
                add(hit, str(comp.relative_to(ROOT)))

    if CREDENTIALS.is_file():
        for line in CREDENTIALS.read_text(errors="replace").splitlines():
            if line.strip().startswith("#"):
                continue
            m = re.search(r"vaultPath:\s*(\S+)", line)
            if m:
                add(m.group(1), "credentials.yaml")

    return found


def main() -> int:
    # Not a skip. Three credentials have already been stored where nothing
    # could read them; a run that reports success without having checked would
    # be the fourth in waiting.
    try:
        import yaml
    except ImportError:
        sys.exit("PyYAML is required: pip install pyyaml")

    if not GOLDEN.is_file():
        sys.exit(f"no golden render at {GOLDEN.relative_to(ROOT)} — "
                 f"the eso-read policy can only be read from one")

    granted, denied = policy_read_paths(GOLDEN.read_text(), yaml)
    if not granted:
        sys.exit(f"found no read grants for policy {ESO_POLICY_NAME!r} in "
                 f"{GOLDEN.relative_to(ROOT)} — refusing to pass every path")

    patterns = collect_patterns()
    if not patterns:
        sys.exit("found no vault paths to check, which is not the same as passing")

    allowed = {waiver_key(k) for k in OPERATOR_ONLY}

    unreadable, waived, stale_waivers, exposed = [], [], [], []
    for path in sorted(patterns):
        concrete = KV_DATA_PREFIX + path
        # Deny first, and deny wins — which is how OpenBao resolves it too. A
        # path inside a granted glob is still unreadable if a deny names it.
        readable = any(vault_glob_matches(g, concrete) for g in granted) and not any(
            vault_glob_matches(d, concrete) for d in denied
        )
        if readable:
            # The inverse finding, and the more serious one. A path declared
            # operator-only that ESO CAN read is a value somebody wrote down as
            # unreachable and that anything able to create an ExternalSecret can
            # reach. Deleting a deny used to produce silence here, which made
            # the deny protecting the escrowed backup keys unguarded by the one
            # lint whose subject is who can read what.
            if path in allowed:
                exposed.append((path, sorted(patterns[path])))
            continue
        if path in allowed:
            waived.append(path)
            continue
        unreadable.append((path, sorted(patterns[path])))

    for key in OPERATOR_ONLY:
        if waiver_key(key) not in patterns:
            stale_waivers.append(key)

    print("")
    print("ESO-readable paths — a stored credential something can read back")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    for path, origins in unreadable:
        print(f"  {RED}ERROR{NC} {path}")
        print(f"        {DIM}built in {', '.join(origins)}{NC}")
        print(f"        {DIM}eso-read cannot read secret/data/{path} — a value{NC}")
        print(f"        {DIM}stored here is written successfully and never materialises.{NC}")
    for path, origins in exposed:
        print(f"  {RED}ERROR{NC} {path}")
        print(f"        {DIM}built in {', '.join(origins)}{NC}")
        print(f"        {DIM}declared operator-only, but eso-read CAN read it. Either the{NC}")
        print(f"        {DIM}deny protecting it was removed, or a grant now covers it.{NC}")
    for key in stale_waivers:
        print(f"  {YELLOW}WARN{NC}  {key}: waived as operator-only, but nothing builds it")
        print(f"        {DIM}Remove it from OPERATOR_ONLY so the list stays a "
              f"statement about this code.{NC}")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    if exposed:
        print(f"{RED}{len(exposed)} path(s) declared operator-only that ESO can read.{NC}")
        print(f"{DIM}Restore the deny in the eso-read policy in{NC}")
        print(f"{DIM}crossplane/compositions/cluster-default.yaml, or remove the{NC}")
        print(f"{DIM}OPERATOR_ONLY entry if the path is genuinely meant to be a Secret.{NC}")
        return 1
    if unreadable:
        print(f"{RED}{len(unreadable)} path(s) ESO cannot read.{NC}")
        print(f"{DIM}Add a named grant to the eso-read policy in{NC}")
        print(f"{DIM}crossplane/compositions/cluster-default.yaml — named, not{NC}")
        print(f"{DIM}tenants/+/*: ESO should read what becomes a Secret, not{NC}")
        print(f"{DIM}everything a tenant owns. If it is not a Secret, add it to{NC}")
        print(f"{DIM}OPERATOR_ONLY with the reason.{NC}")
        return 1
    print(f"{GREEN}Every vault path is readable by ESO or declared operator-only{NC} "
          f"({len(patterns)} path(s), {len(waived)} operator-only).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
