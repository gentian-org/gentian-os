#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-cluster-config-keys.py — does everyone agree on the key name?
# =============================================================================
# gentian-cluster-config is a contract with no enforcement on either side.
#
# The producer is the Cluster Composition. The consumers are
# Compositions, which read it through function-extra-resources and pull values
# out with sprig's `dig`:
#
#   {{- $kubeApiCIDR := dig "network.kubeApiServerCidr" "" $ccData }}
#
# `dig` takes a default. Every read in every Composition supplies "" or a
# literal, so a key that is missing, renamed or misspelled is not an error — it
# is the empty string, silently. Nothing fails, nothing warns, and the value
# lands in whatever the Composition renders: an ipBlock with no CIDR, a
# LimitRange with no limit, an init Job with no resources.
#
# The render fixtures do not cover it either. tenant-default reads
# `llm.enabled` and `network.kubeApiServerEndpointPort`; its fixture supplied
# neither, and check-render-fixtures passed. Removing a key the Composition
# demonstrably reads left all ten fixtures green.
#
# There are two consumers, not one. Compositions read it through
# function-extra-resources, and the operator reads it directly — see
# internal/controller/cluster_config.go, whose whole point is that "the operator
# and the Compositions decide from the same source instead of from a Helm value
# that has to be kept in agreement by hand". A lint that knew only about
# Compositions reported llm.enabled as read by nobody while the operator was
# reading it, and would have been blind to the reverse.
#
# So the agreement is checked here instead. Four tiers, by how exactly the
# answer can be known:
#
#   ERROR  A Composition reads a key the producer does not write. Both sides
#          name the key in text, so a disagreement is a fact. This is the
#          direction that silently yields "".
#   ERROR  The operator reads a key the producer does not write. Quieter still:
#          a missing key is `ok == false`, so the reader falls back to its env
#          var and the cluster runs on the Helm value the ConfigMap exists to
#          replace, with nothing to show for it. That is how mail.serviceMode
#          came to read kernel in the claim and external in the operator.
#   WARN   The producer writes a key nothing reads. Probably dead, but a reader
#          using a form this does not recognise would make that a false
#          accusation, so it is reported and never fatal.
#   WARN   A render fixture omits a key its Composition reads, which makes the
#          fixture pass while exercising the empty-string path instead of the
#          one it appears to test.
#
# Usage:
#   scripts/lint/lint-cluster-config-keys.py [--strict]
#
#   --strict  treat warnings as failures
# =============================================================================

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PRODUCER = ROOT / "crossplane" / "compositions" / "cluster-default.yaml"
COMPOSITIONS = sorted((ROOT / "crossplane" / "compositions").glob("*.yaml"))
FIXTURES = sorted((ROOT / "crossplane" / "tests" / "unit" / "render").glob("*/required-resources/cluster-config.yaml"))
# The operator reads the same ConfigMap, and is the second consumer this lint
# has to know about. Without it, a key only Go reads looks unread — llm.enabled
# was reported as carried by nobody while cluster_config.go was reading it — and
# a key only Go reads but nothing writes is invisible, which is the direction
# that silently falls through to an env var.
GO_SOURCES = sorted((ROOT / "internal").rglob("*.go"))

CONFIGMAP_NAME = "gentian-cluster-config"

# The ConfigMap the Composition emits, and the key lines in its data block.
# Keys are dotted and unquoted on the left of a colon, e.g.
#   node.ip: {{ default "" (dig "nodeIp" "" $spec) | quote }}
PRODUCER_ANCHOR = "name: gentian-cluster-config"
KEY_LINE = re.compile(r'^\s{20,}([A-Za-z][A-Za-z0-9.]*): ')

# A Composition read: dig "<key>" <default> $ccData — the variable holding the
# ConfigMap's .data map. Matching on that variable is what separates a
# cluster-config read from a dig against some other map, such as
# `dig "data" "objects.json" "[]" $provCM`.
CC_READ = re.compile(r'dig\s+"([A-Za-z][A-Za-z0-9.]*)"\s+\S+\s+\$(?:ccData|clusterConfigData)\b')

# A Go read. Two forms, both anchored on the file naming the ConfigMap so this
# does not collect keys from unrelated maps:
#   clusterConfigLLMKey = "llm.enabled"     — the declared-constant form
#   cm.Data["mail.serviceMode"]             — read inline
GO_KEY_CONST = re.compile(r'clusterConfig\w*Key\s*=\s*"([A-Za-z][A-Za-z0-9.]*)"')
GO_KEY_INLINE = re.compile(r'\.Data\[\s*"([A-Za-z][A-Za-z0-9.]*)"\s*\]')


def producer_keys() -> set[str]:
    """Keys the Cluster Composition writes, read out of the ConfigMap it emits."""
    text = PRODUCER.read_text().splitlines()
    try:
        start = next(i for i, line in enumerate(text)
                     if line.strip() == PRODUCER_ANCHOR)
    except StopIteration:
        sys.exit(
            f"FATAL: the {CONFIGMAP_NAME} ConfigMap was not found in "
            f"{PRODUCER.relative_to(ROOT)}.\n"
            "       If the producer moved, point this lint at the new one rather\n"
            "       than deleting it: the agreement it checks matters more after\n"
            "       that move, not less."
        )
    keys, in_data = set(), False
    for line in text[start:]:
        stripped = line.strip()
        if in_data:
            # The data block ends at the next key of the enclosing mapping.
            if stripped.startswith(("providerConfigRef:", "---")):
                break
            m = KEY_LINE.match(line)
            if m:
                keys.add(m.group(1))
        elif stripped == "data:":
            in_data = True
    return keys


def consumer_keys() -> dict[str, set[str]]:
    """Keys each Composition reads from the ConfigMap."""
    out: dict[str, set[str]] = {}
    for path in COMPOSITIONS:
        found = set(CC_READ.findall(path.read_text()))
        if found:
            out[str(path.relative_to(ROOT))] = found
    return out


def go_consumer_keys() -> dict[str, set[str]]:
    """Keys the operator reads from the ConfigMap, per file.

    Scoped to files that name the ConfigMap or its constant, so a .Data[...]
    read of some other map is not mistaken for one of these.
    """
    out: dict[str, set[str]] = {}
    for path in GO_SOURCES:
        text = path.read_text()
        if CONFIGMAP_NAME not in text and "clusterConfigName" not in text:
            continue
        found = set(GO_KEY_CONST.findall(text)) | {
            k for k in GO_KEY_INLINE.findall(text) if "." in k
        }
        if found:
            out[str(path.relative_to(ROOT))] = found
    return out


def fixture_keys(path: Path) -> set[str]:
    """Keys a render fixture's fake ConfigMap supplies."""
    keys, in_data = set(), False
    for line in path.read_text().splitlines():
        if line.startswith("data:"):
            in_data = True
            continue
        if in_data:
            if line and not line.startswith((" ", "\t")):
                break
            m = KEY_LINE.match(line)
            if m:
                keys.add(m.group(1))
    return keys


def main() -> int:
    strict = "--strict" in sys.argv
    written = producer_keys()
    read_by = consumer_keys()
    go_read_by = go_consumer_keys()
    if not written:
        print(f"FATAL: no keys parsed from {PRODUCER_FUNC}", file=sys.stderr)
        return 1
    if not read_by:
        print("FATAL: no Composition reads parsed — has the read form changed?", file=sys.stderr)
        return 1

    errors: list[str] = []
    warnings: list[str] = []

    # ERROR — read but never written. This is the silent-empty-string direction.
    for path, keys in read_by.items():
        for key in sorted(keys - written):
            errors.append(
                f"{path} reads {key!r}, which nothing writes to {CONFIGMAP_NAME}.\n"
                f"    dig returns the default, so this renders empty instead of failing."
            )

    # ERROR — the same direction in Go, which fails even more quietly. A missing
    # key is `ok == false`, so the reader falls back to its env var and the
    # cluster runs on the Helm value the ConfigMap was meant to replace. That is
    # not a hypothetical: it is how mail.serviceMode came to say kernel in the
    # claim and external in the operator.
    for path, keys in go_read_by.items():
        for key in sorted(keys - written):
            errors.append(
                f"{path} reads {key!r}, which nothing writes to {CONFIGMAP_NAME}.\n"
                f"    A missing key is not an error to the reader — it falls back to its\n"
                f"    env var, so the claim stops deciding and nothing says so."
            )

    all_read = set().union(*read_by.values()) | (
        set().union(*go_read_by.values()) if go_read_by else set()
    )

    # WARN — written but never read.
    for key in sorted(written - all_read):
        warnings.append(f"{CONFIGMAP_NAME} carries {key!r}, which nothing reads.")

    # WARN — a fixture that does not supply what its Composition reads is
    # testing the empty-string path while appearing to test the real one.
    for fixture in FIXTURES:
        case = fixture.parent.parent.name
        supplied = fixture_keys(fixture)
        composition = (fixture.parent.parent / "composition.yaml")
        if not composition.exists():
            continue
        needed = set(CC_READ.findall(composition.read_text()))
        for key in sorted(needed - supplied):
            warnings.append(
                f"render fixture {case!r} omits {key!r}, which its composition reads — "
                f"the case passes on the empty-string path."
            )

    for w in warnings:
        print(f"\033[0;33mWARN\033[0m  {w}")
    for e in errors:
        print(f"\033[0;31mERROR\033[0m {e}", file=sys.stderr)

    if errors:
        print(f"\n{len(errors)} key(s) read but never written.", file=sys.stderr)
        return 1
    if warnings and strict:
        print(f"\n{len(warnings)} warning(s), --strict.", file=sys.stderr)
        return 1
    print(f"\033[0;32mEvery cluster-config key a reader asks for is written\033[0m "
          f"({len(all_read)} read — {len(set().union(*read_by.values()))} by Compositions, "
          f"{len(set().union(*go_read_by.values())) if go_read_by else 0} by the operator — "
          f"{len(written)} written).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
