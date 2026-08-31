#!/usr/bin/env python3
# =============================================================================
# scripts/tools/verify-argocd-config.py — the cluster's Argo CD is configured
# the way the installer configures one
# =============================================================================
# Argo CD's own settings are not under Argo CD. argocd-cm, argocd-cmd-params-cm
# and argocd-rbac-cm are written by scripts/lib/argocd.sh with `kubectl patch`
# during install, and nothing reconciles them afterwards.
#
# That is a deliberate choice rather than an oversight. These are bootstrap
# settings: they must be right before the thing that would reconcile them is
# trustworthy, and an Application that manages argocd-cm can leave the
# controller unable to fix itself. The installer keeps them.
#
# The cost of keeping them there is that nothing notices when they diverge. A
# key patched onto a running cluster and never added to the script is lost on
# the next cluster; a key added to the script after this cluster was built is
# absent here. Either way both look fine, because neither side is checked
# against the other and no controller is unhappy.
#
# The same shape has already cost this platform twice: mail.serviceMode said one
# thing in the claim and another on the cluster for as long as nobody looked
# (see verify-claim-applied.sh), and the tenant sequencer named a resource that
# had moved seven weeks earlier while every composite read healthy.
#
# WHAT IT CHECKS
#
# Every ConfigMap key scripts/lib/argocd.sh patches must be present on the
# cluster with the same value.
#
# WHAT IT DOES NOT CHECK
#
# Values in patches the script builds with shell interpolation — the OIDC config
# and the RBAC policy, which carry the cluster's own domain and group names.
# Those are reported as present-or-absent only, and say so, because a literal
# comparison against an uninterpolated `${VAR}` would fail every time and a
# check that always fails is a check nobody reads.
#
# Nor keys whose NAME is interpolated. argocd-tls-certs-cm is keyed by
# `id.${kernel_domain}`, which is not a key until a cluster exists to name it,
# so that patch contributes nothing here and its absence would go unreported.
#
# Nor does it check keys the cluster has and the script does not. Argo ships its
# own defaults in these ConfigMaps and an operator may reasonably add more; this
# asserts the installer's intent is present, not that nothing else is.
#
# Reads only. It reports disagreement; it does not patch, because the right
# repair differs — a key the script gained wants applying here, a key this
# cluster gained wants adding to the script so the next cluster gets it.
#
# Usage:
#   python3 scripts/tools/verify-argocd-config.py
# =============================================================================
import json
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "scripts/lib/argocd.sh"
NAMESPACE = "argocd"

RED = "\033[0;31m"
GREEN = "\033[0;32m"
YELLOW = "\033[0;33m"
DIM = "\033[2m"
NC = "\033[0m"

# `kubectl patch configmap <name> -n argocd --type merge [\] -p '<json>'`
# The payload is single-quoted when static and double-quoted when the script
# interpolates a shell variable into it; both forms appear in argocd.sh.
PATCH_STATIC = re.compile(
    r"kubectl\s+(?:-n\s+\S+\s+)?patch\s+configmap\s+(?P<cm>[a-z0-9-]+)"
    r"(?:\s+-n\s+\S+)?\s+--type\s+merge\s*\\?\s*-p\s+'(?P<body>[^']*)'",
    re.S,
)
# Every patch invocation, whatever the payload's quoting. Keys are read out of
# the region that follows rather than by matching the payload as a string: the
# interpolated ones embed `$(jq ... <<<"${var}")`, whose bare quote ends any
# regex that tries to honour shell quoting. Region-scanning does not care.
PATCH_ANY = re.compile(
    r"kubectl\s+(?:-n\s+\S+\s+)?patch\s+configmap\s+(?P<cm>[a-z0-9-]+)"
    r"(?:\s+-n\s+\S+)?\s+--type\s+merge",
)
# Where one invocation's payload stops. Without this the scan runs on into the
# next patch and credits its keys to the wrong ConfigMap — which it did, and
# reported three confident findings against argocd-tls-certs-cm that belong to
# argocd-cm.
NEXT_COMMAND = re.compile(r"^\s*(kubectl|local|fi|\})\s", re.M)
# A key in the payload, with or without the backslashes a double-quoted heredoc
# puts in front of its quotes.
DATA_KEY = re.compile(r'\\?"([a-zA-Z][a-zA-Z0-9._/-]*)\\?"\s*:')


def declared():
    """What argocd.sh patches: {configmap: {key: value-or-None}}.

    A None value means the patch interpolates a shell variable, so the value
    here cannot be compared with the cluster's — only the key's presence.
    """
    src = INSTALLER.read_text()
    out: dict[str, dict[str, str | None]] = {}

    for m in PATCH_STATIC.finditer(src):
        try:
            body = json.loads(m.group("body"))
        except json.JSONDecodeError:
            continue
        data = body.get("data")
        if isinstance(data, dict):
            out.setdefault(m.group("cm"), {}).update(data)

    # Second pass over every invocation, to pick up keys the first could not
    # parse. A key already known from a static patch keeps its exact value; one
    # seen only here is recorded as present-but-not-comparable.
    for m in PATCH_ANY.finditer(src):
        region = src[m.end():m.end() + 2000]
        # Stop at whichever comes first: the line closing the payload, or the
        # next shell statement.
        end = re.search(r"^\s*\}[\"']", region, re.M)
        nxt = NEXT_COMMAND.search(region)
        cut = min(x.end() for x in (end, nxt) if x) if (end or nxt) else len(region)
        region = region[:cut]
        if not re.search(r'\\?"data\\?"', region):
            continue
        inner = re.split(r'\\?"data\\?"', region, maxsplit=1)[-1]
        for key in DATA_KEY.findall(inner):
            out.setdefault(m.group("cm"), {}).setdefault(key, None)

    return out


def live(configmap: str):
    """The cluster's copy, or None when the ConfigMap is absent."""
    proc = subprocess.run(
        ["kubectl", "get", "configmap", configmap, "-n", NAMESPACE, "-o", "json"],
        capture_output=True, text=True,
    )
    if proc.returncode != 0:
        return None
    return json.loads(proc.stdout).get("data") or {}


def main() -> int:
    if shutil.which("kubectl") is None:
        print(f"{YELLOW}SKIP{NC} — kubectl not found.")
        return 0
    if subprocess.run(["kubectl", "cluster-info"],
                      capture_output=True).returncode != 0:
        print(f"{YELLOW}SKIP{NC} — no reachable cluster.")
        return 0
    if not INSTALLER.is_file():
        sys.exit(f"{INSTALLER.relative_to(ROOT)} is missing — nothing to check "
                 f"against, which is not the same as passing")

    wanted = declared()
    if not wanted:
        sys.exit("parsed no ConfigMap patches out of scripts/lib/argocd.sh — "
                 "the patch form has probably changed, and a check that finds "
                 "nothing would pass every cluster")

    missing, differing, unchecked, checked = [], [], [], 0

    print("")
    print("Argo CD configuration — the cluster carries what the installer sets")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")

    for cm in sorted(wanted):
        current = live(cm)
        if current is None:
            missing.append((cm, "<the ConfigMap itself>"))
            continue
        for key, value in sorted(wanted[cm].items()):
            if key not in current:
                missing.append((cm, key))
            elif value is None:
                unchecked.append((cm, key))
            elif current[key] != value:
                differing.append((cm, key, value, current[key]))
            else:
                checked += 1

    for cm, key in missing:
        print(f"  {RED}MISSING{NC} {cm}: {key}")
        print(f"          {DIM}the installer sets this and the cluster has "
              f"not got it{NC}")
    for cm, key, want, got in differing:
        print(f"  {RED}DIFFERS{NC} {cm}: {key}")
        print(f"          {DIM}installer: {want[:70]!r}{NC}")
        print(f"          {DIM}cluster:   {got[:70]!r}{NC}")
    for cm, key in unchecked:
        print(f"  {YELLOW}VALUE NOT CHECKED{NC} {cm}: {key}")
        print(f"          {DIM}patched with shell interpolation — present, "
              f"value not comparable{NC}")

    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    if missing or differing:
        print(f"{RED}{len(missing) + len(differing)} setting(s) disagree.{NC}")
        print(f"{DIM}Nothing reconciles these ConfigMaps, so neither side is{NC}")
        print(f"{DIM}authoritative by default. A key the script gained wants{NC}")
        print(f"{DIM}applying here; a key this cluster gained wants adding to{NC}")
        print(f"{DIM}scripts/lib/argocd.sh, or the next cluster is built without it.{NC}")
        return 1
    print(f"{GREEN}Every setting the installer writes is on the cluster{NC} "
          f"({checked} compared, {len(unchecked)} present but not comparable).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
