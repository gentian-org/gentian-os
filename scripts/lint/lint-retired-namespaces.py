#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-retired-namespaces.py - a namespace the layout no longer
# has is named nowhere
# =============================================================================
# kernel/namespaces.yaml is the layout: kernel-<function>, system-<function>,
# tenant-<t> and tenant-<t>-dmz. The names it replaced kept turning up as the
# default of a variable, the fallback of a template and the literal in a
# manifest nothing substitutes. Each is right for as long as its caller passes
# the real name and wrong the first time one does not: a wait that looks in a
# namespace this cluster never had, a Secret reference nothing can resolve, a
# purge that walks past every volume.
#
# lint-namespace-layout.sh keeps the old names out of the steps and the
# bootstrap chart. This reads every tracked file.
#
# Two kinds of name:
#
#   RETIRED   names that were only ever a namespace. Refused wherever they
#             appear.
#   REUSED    names a chart, a release, a ServiceAccount or a ClusterSecretStore
#             still carries (openbao, argocd, cert-manager, ...). Refused only
#             where the text addresses a namespace: `namespace: x`, `-n x`,
#             `--namespace x`, `<svc>.x.svc`, `${..._NAMESPACE:-x}`,
#             `serviceaccount:x:` and a Go Namespace field or variable.
#
# A line that is only a comment is not read: saying what a name used to be is
# what comments here are for. Documentation is not read either.
#
# What is left is ALLOWED below, each entry with its reason. An entry that no
# longer matches anything fails too, so the list cannot outlive what it
# excuses.
# =============================================================================

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

RETIRED = [
    "platform-kernel",
    "gentian-system",
    "crossplane-system",
    "cnpg-system",
    "stakater-system",
    "envoy-gateway-system",
]
# The stage-suffixed pair, gentian-<stage> and gentian-infra-<stage>, however
# the stage is spelled: a literal, a shell variable, a template value, a
# format verb.
STAGE = r"(?:dev|prod|staging|\$\{?(?:ENV|env|STAGE|stage)\b|\{\{-? *\.Values\.env|<env>|<stage>|%s)"

REUSED = [
    "argocd-image-updater",
    "external-secrets",
    "cert-manager",
    "external-dns",
    "openbao",
    "argocd",
    "kyverno",
]

_retired = "|".join(re.escape(n) for n in RETIRED)
_reused = "|".join(re.escape(n) for n in REUSED)

PATTERNS = [
    # Not inside a longer name (cloudflare-api-token-gentian-system) and not a
    # mail address (gentian-system@<domain> is the kernel realm's sender).
    re.compile(rf"(?<!\w)(?<!\w-)(?:{_retired})(?![\w@-])"),
    re.compile(rf"""(?<!\w)(?<!\w-)gentian-infra-(?:{STAGE}|["']\s*\+)"""),
    re.compile(rf"(?<![\w.])(?<!\w-)gentian-{STAGE}(?![\w@-])"),
    re.compile(rf"""namespace["']?\s*[:=]\s*["']?(?:{_reused})["']?\s*(?:[,}}#].*)?$""", re.I),
    re.compile(rf"""(?:\s-n|--namespace)[\s=]+["']?(?:{_reused})(?![\w-])"""),
    re.compile(rf"\.(?:{_reused})\.svc\b"),
    re.compile(rf"(?:NAMESPACE|_NS)\w*:[-=](?:{_reused})\}}"),
    re.compile(rf"serviceaccount:(?:{_reused}):"),
    re.compile(rf"""N(?:amespace|S)\w*\s*:?=\s*"(?:{_reused})\""""),
]

# Not read at all.
SKIPPED_PREFIXES = (
    "docs/",
    # Catalogue profiles as another repository published them, kept to test
    # the bundle reader against real input.
    "internal/profilebundle/testdata/",
)
SKIPPED_SUFFIXES = (".md", ".sum", ".png", ".svg", ".ico", ".jar", ".tgz")
SKIPPED_FILES = {
    "scripts/lint/lint-retired-namespaces.py",
}

# path, or path prefix ending in "/" or "-" -> [(text the line contains, why
# the old name is the point there)]
ALLOWED = {
    "scripts/lint/lint-namespace-layout.sh": [
        ("old='", "the names that lint refuses"),
    ],
}

COMMENT = re.compile(r"^\s*(#|//|\*|/\*|\{\{-?\s*/\*)")


def tracked():
    out = subprocess.run(
        ["git", "ls-files", "-z"], cwd=ROOT, check=True, capture_output=True
    ).stdout.decode()
    for name in out.split("\0"):
        if not name or name in SKIPPED_FILES:
            continue
        if name.startswith(SKIPPED_PREFIXES) or name.endswith(SKIPPED_SUFFIXES):
            continue
        yield name


def allowed_keys(name):
    """The ALLOWED entries that cover a path: its own, and any prefix's."""
    return [k for k in ALLOWED if name == k or (k[-1] in "/-" and name.startswith(k))]


def template_comment_lines(lines):
    """Line numbers inside a Go template comment that spans lines."""
    inside, out = False, set()
    for i, line in enumerate(lines):
        if not inside and re.search(r"\{\{-?\s*/\*", line):
            inside = True
        if inside:
            out.add(i)
            if "*/" in line:
                inside = False
    return out


def main():
    hits, used = [], set()
    for name in tracked():
        path = ROOT / name
        # A render fixture's composition.yaml is a link to the Composition.
        if path.is_symlink() or not path.is_file():
            continue
        try:
            lines = path.read_text().split("\n")
        except UnicodeDecodeError:
            continue
        in_comment = template_comment_lines(lines)
        for i, line in enumerate(lines):
            if i in in_comment or COMMENT.match(line):
                continue
            if not any(p.search(line) for p in PATTERNS):
                continue
            excused = False
            for key in allowed_keys(name):
                for n, (text, _why) in enumerate(ALLOWED[key]):
                    if text in line:
                        used.add((key, n))
                        excused = True
                        break
            if not excused:
                hits.append(f"{name}:{i + 1}: {line.strip()[:160]}")

    stale = [
        f"{name}: {text!r}"
        for name, entries in ALLOWED.items()
        for n, (text, _why) in enumerate(entries)
        if (name, n) not in used
    ]

    for hit in hits:
        print(f"FAIL - a retired namespace name: {hit}", file=sys.stderr)
    for entry in stale:
        print(f"FAIL - ALLOWED excuses a line that is no longer there: {entry}", file=sys.stderr)
    if hits:
        print(
            "\nThe layout is kernel/namespaces.yaml. Ask for the namespace by function\n"
            "(ns_kernel / ns_system in shell, layout.Namespace / layout.System in Go,\n"
            'include "ns" in the bootstrap chart), or name the one the layout has.',
            file=sys.stderr,
        )
    if hits or stale:
        return 1
    print("OK - no retired namespace name outside the allowlist.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
