#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Gentian Organization
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Render the operator ClusterRole in the Helm chart from the RBAC rules
controller-gen derives from the +kubebuilder:rbac markers in ./internal/...

The chart's ClusterRole used to be maintained by hand while the markers sat
beside the code doing nothing — controller-gen was only ever invoked for
deepcopy and CRDs, and only over ./api/..., which contains no rbac markers at
all. The two drifted, and every gap surfaced as a different mystery: a wedged
tenant reconcile (oidcpackcatalogs), a CrashLoopBackOff with no hint of RBAC
(appcatalogues), a controller that never converged (referencegrants), a tenant
NetworkPolicy that silently blocked the apiserver (endpointslices). Eleven of
them. This script makes the markers the single source of truth.

It deliberately re-normalises the rules rather than copying controller-gen's
output verbatim: rules are regrouped by (apiGroups, verbs) and sorted into a
canonical order here. A controller-gen upgrade that changes grouping or key
order therefore cannot produce a spurious diff in the chart — only an actual
change in the permission set can.
"""

from __future__ import annotations

import pathlib
import sys
from collections import defaultdict

import yaml

REPO = pathlib.Path(__file__).resolve().parent.parent
ROLE = REPO / "config" / "rbac" / "role.yaml"
OUT = REPO / "charts" / "gentian-os" / "templates" / "clusterrole.yaml"

# Verb order matches how a human reads a rule (reads, then writes) rather than
# alphabetical, so the rendered chart stays reviewable in a diff.
VERB_ORDER = ["get", "list", "watch", "create", "update", "patch", "delete"]

HEADER = """{{- /*
  GENERATED FILE — DO NOT EDIT.

  Rendered by scripts/gen-clusterrole.py from the +kubebuilder:rbac markers in
  internal/..., via controller-gen. To change a permission, edit the marker next
  to the code that needs it and run `make gen-all`. Editing this file by hand
  will be reverted by the next generation run, and CI's verify-gen job fails on
  any drift between the two.

  The rationale for each grant lives with its marker, where it stays next to the
  code that depends on it.
*/ -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "gentian-os.fullname" . }}
  labels:
    {{- include "gentian-os.labels" . | nindent 4 }}
rules:
"""


def sort_verbs(verbs: set[str]) -> list[str]:
    """Known verbs in reading order, then anything unrecognised alphabetically."""
    known = [v for v in VERB_ORDER if v in verbs]
    rest = sorted(v for v in verbs if v not in VERB_ORDER)
    return known + rest


def group_key(group: str) -> tuple[int, str]:
    """Core ("") first, then alphabetical — matches how the rules read."""
    return (0 if group == "" else 1, group)


def canonicalise(rules: list[dict]) -> list[dict]:
    """Merge rules sharing an apiGroup+verb set, then sort deterministically."""
    merged: dict[tuple[str, tuple[str, ...]], set[str]] = defaultdict(set)
    for rule in rules:
        verbs = tuple(sort_verbs(set(rule.get("verbs", []))))
        for group in rule.get("apiGroups", []):
            merged[(group, verbs)].update(rule.get("resources", []))

    out = []
    for (group, verbs), resources in merged.items():
        out.append(
            {
                "apiGroups": [group],
                "resources": sorted(resources),
                "verbs": list(verbs),
            }
        )
    out.sort(key=lambda r: (group_key(r["apiGroups"][0]), r["resources"]))
    return out


def render(rules: list[dict]) -> str:
    body = yaml.safe_dump(
        rules, default_flow_style=False, sort_keys=False, width=10**6
    )
    # safe_dump emits top-level list items unindented; the chart nests them
    # under `rules:`, so indent by two.
    body = "\n".join(("  " + line) if line.strip() else line for line in body.splitlines())
    return HEADER + body + "\n"


def main() -> int:
    if not ROLE.exists():
        print(
            f"error: {ROLE.relative_to(REPO)} not found — run `make manifests` "
            "so controller-gen generates it first.",
            file=sys.stderr,
        )
        return 1

    doc = yaml.safe_load(ROLE.read_text())
    if not doc or doc.get("kind") != "ClusterRole":
        print(f"error: {ROLE.relative_to(REPO)} is not a ClusterRole", file=sys.stderr)
        return 1

    rules = canonicalise(doc.get("rules") or [])
    if not rules:
        # An empty rule set means controller-gen scanned the wrong paths. Writing
        # it out would silently strip every permission from the operator, so
        # refuse rather than generate a role that breaks the cluster.
        print(
            "error: controller-gen produced no rules — check that the rbac "
            "paths in the Makefile still point at the packages holding the "
            "+kubebuilder:rbac markers.",
            file=sys.stderr,
        )
        return 1

    OUT.write_text(render(rules))
    print(f"wrote {OUT.relative_to(REPO)} ({len(rules)} rules)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
