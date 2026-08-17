#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-cluster-config-keys.py — does everyone agree on the key name?
# =============================================================================
# gentian-cluster-config is a contract with no enforcement on either side.
#
# The producer is a heredoc in scripts/lib/common.sh. The consumers are
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
# So the agreement is checked here instead. Three tiers, by how exactly the
# answer can be known:
#
#   ERROR  A Composition reads a key the producer does not write. Both sides
#          name the key in text, so a disagreement is a fact. This is the
#          direction that silently yields "".
#   WARN   The producer writes a key no Composition reads. Probably dead, but a
#          reader using a form this does not recognise would make that a false
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
PRODUCER = ROOT / "scripts" / "lib" / "common.sh"
COMPOSITIONS = sorted((ROOT / "crossplane" / "compositions").glob("*.yaml"))
FIXTURES = sorted((ROOT / "crossplane" / "tests" / "unit" / "render").glob("*/required-resources/cluster-config.yaml"))

CONFIGMAP_NAME = "gentian-cluster-config"

# The producer function and the ConfigMap key lines inside its heredoc. Keys are
# dotted and unquoted on the left of a colon, two spaces in, e.g.
#   network.routingMode: "${_routing_mode}"
PRODUCER_FUNC = "upsert_gentian_cluster_config()"
KEY_LINE = re.compile(r'^  ([A-Za-z][A-Za-z0-9.]*): ')

# A Composition read: dig "<key>" <default> $ccData — the variable holding the
# ConfigMap's .data map. Matching on that variable is what separates a
# cluster-config read from a dig against some other map, such as
# `dig "data" "objects.json" "[]" $provCM`.
CC_READ = re.compile(r'dig\s+"([A-Za-z][A-Za-z0-9.]*)"\s+\S+\s+\$(?:ccData|clusterConfigData)\b')


def producer_keys() -> set[str]:
    """Keys the installer writes, read out of the heredoc in common.sh."""
    text = PRODUCER.read_text().splitlines()
    try:
        start = next(i for i, line in enumerate(text) if line.startswith(PRODUCER_FUNC))
    except StopIteration:
        sys.exit(
            f"FATAL: {PRODUCER_FUNC} not found in {PRODUCER.relative_to(ROOT)}.\n"
            "       If the producer moved — for example into the Cluster Composition —\n"
            "       point this lint at the new one rather than deleting it: the\n"
            "       agreement it checks matters more after that move, not less."
        )
    keys, in_heredoc = set(), False
    for line in text[start:]:
        if line.startswith("}"):
            break
        if "<<EOF" in line:
            in_heredoc = True
            continue
        if in_heredoc:
            if line.startswith("EOF"):
                break
            m = KEY_LINE.match(line)
            # Skip ConfigMap metadata, which uses the same shape as data keys.
            if m and m.group(1) not in {"name", "namespace", "labels", "annotations"}:
                keys.add(m.group(1))
    return keys


def consumer_keys() -> dict[str, set[str]]:
    """Keys each Composition reads from the ConfigMap."""
    out: dict[str, set[str]] = {}
    for path in COMPOSITIONS:
        found = set(CC_READ.findall(path.read_text()))
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

    all_read = set().union(*read_by.values())

    # WARN — written but never read.
    for key in sorted(written - all_read):
        warnings.append(f"{CONFIGMAP_NAME} carries {key!r}, which no Composition reads.")

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
    print(f"\033[0;32mEvery cluster-config key a Composition reads is written\033[0m "
          f"({len(all_read)} read, {len(written)} written).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
