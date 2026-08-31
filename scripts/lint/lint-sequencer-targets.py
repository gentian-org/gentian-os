#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-sequencer-targets.py — a sequencer step that waits forever
# =============================================================================
# function-sequencer orders composed resources: "create namespace, then
# limitrange, then vault-policy". It does that by holding back everything after
# a name until a resource matching that name exists.
#
# If nothing ever matches the name, everything after it is held back forever.
#
# That is not an error state. The sequencer reports a normal delay:
#
#   Pipeline step "sequence-tenant-provisioning": Delaying creation of
#   resource(s) matching "vault-policy" because "networkpolicy" does not
#   exist yet
#
# and the composite stays Synced=True, Ready=True, because delaying is what the
# sequencer is for. Nothing is broken from Crossplane's point of view.
#
# On 2026-07-13 the tenant NetworkPolicy moved out of tenant-default.yaml and
# into the operator (tenant_network_policy.go). The composed resource went; the
# `networkpolicy` entry in the sequencer rule stayed. For the seven weeks that
# followed, `vault-policy` — the entry after it — was never created, so no
# tenant had the OpenBao policy that its administrator's token names, and every
# tenant-scoped credential write answered
#
#   403 permission denied
#
# on a cluster where every composite read healthy. It surfaced when someone
# tried to set a backup destination and read the 403 as an authorization
# decision, which is exactly what it looks like.
#
# WHAT IT CHECKS
#
# Every name in every function-sequencer rule must match at least one composed
# resource the Composition actually renders. Rendered names come from the golden
# fixtures in crossplane/tests/unit/render/, which carry real post-template
# names — `job-keycloak-realm-render-fixture`, not `job-{{ $job.metadata.name }}`
# — so a templated name is checked as the cluster would see it. Fixtures for the
# same Composition are unioned, because one scenario need not exercise every
# conditional resource.
#
# The entries are regexes. Which flavour of match the sequencer performs is its
# business, so a name passes if it matches under any of fullmatch, match or
# search: the aim is to catch a name that matches nothing at all, not to
# second-guess the sequencer's dialect on names that match something.
#
# WHAT IT DOES NOT CHECK
#
# That the ORDER is right — only that each name refers to something. A sequence
# naming two real resources in a useless order is a design question a lint
# cannot answer.
#
# Nor does it check a Composition with sequencer rules and no fixture. It
# reports that as an error rather than skipping it, because a check that passes
# when it did not run reads as coverage.
#
# Usage:
#   python3 scripts/lint/lint-sequencer-targets.py
# =============================================================================
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
COMPOSITIONS = ROOT / "crossplane/compositions"
RENDER_DIR = ROOT / "crossplane/tests/unit/render"

RED = "\033[0;31m"
GREEN = "\033[0;32m"
YELLOW = "\033[0;33m"
DIM = "\033[2m"
NC = "\033[0m"

SEQUENCER_KIND = "Input"
SEQUENCER_API_PREFIX = "sequencer.fn.crossplane.io"

# The annotation is written by the templating function as
# `gotemplating.fn.crossplane.io/composition-resource-name` and comes back on the
# rendered object as `crossplane.io/composition-resource-name`. Match on the
# suffix so this reads whichever side it is handed, rather than silently finding
# no names and reporting every entry as dead — which is how this check first
# behaved, and it looked exactly like a real finding.
RESOURCE_NAME_SUFFIX = "composition-resource-name"


def composition_name(doc):
    """The Composition's metadata.name, which is how fixtures identify it."""
    if not isinstance(doc, dict) or doc.get("kind") != "Composition":
        return None
    return (doc.get("metadata") or {}).get("name")


def sequencer_rules(doc):
    """Every (step, [names]) sequence declared by a Composition's pipeline.

    Read from the parsed Composition rather than by grepping: a rule is nested
    under pipeline[].input.rules[].sequence, and a name that happens to appear
    in a comment or in a template body is not a rule.
    """
    out = []
    for step in (doc.get("spec") or {}).get("pipeline") or []:
        if not isinstance(step, dict):
            continue
        inp = step.get("input")
        if not isinstance(inp, dict):
            continue
        if not str(inp.get("apiVersion", "")).startswith(SEQUENCER_API_PREFIX):
            continue
        if inp.get("kind") != SEQUENCER_KIND:
            continue
        for rule in inp.get("rules") or []:
            if isinstance(rule, dict) and isinstance(rule.get("sequence"), list):
                names = [str(n) for n in rule["sequence"]]
                out.append((step.get("step", "<unnamed step>"), names))
    return out


def rendered_names(expected_path, yaml):
    """The composition-resource-names one fixture's golden render contains.

    These are post-template: the annotation on a rendered object carries the
    name the sequencer will actually see.
    """
    names = set()
    try:
        docs = list(yaml.safe_load_all(expected_path.read_text()))
    except yaml.YAMLError as exc:
        return names, f"cannot parse {expected_path.name}: {exc}"
    for doc in docs:
        if not isinstance(doc, dict):
            continue
        ann = (doc.get("metadata") or {}).get("annotations") or {}
        for key, value in ann.items():
            if str(key).rsplit("/", 1)[-1] == RESOURCE_NAME_SUFFIX and value:
                names.add(str(value))
    return names, None


def matches_any(entry, names):
    """Whether a sequencer entry selects at least one rendered name.

    Permissive on the regex flavour deliberately — see the header. A bad
    pattern is reported as itself rather than as a missing resource, because
    "this is not a regex" and "this names nothing" are different repairs.
    """
    try:
        pattern = re.compile(entry)
    except re.error as exc:
        return None, f"not a valid regex: {exc}"
    for name in names:
        if pattern.fullmatch(name) or pattern.match(name) or pattern.search(name):
            return True, None
    return False, None


def main() -> int:
    # Not a skip. The failure this exists for is silent by construction, so a
    # run that reports success without having checked anything would be the
    # same shape of lie. Same stance as lint-composed-resource-names.py.
    try:
        import yaml
    except ImportError:
        sys.exit("PyYAML is required: pip install pyyaml")

    if not COMPOSITIONS.is_dir():
        sys.exit(f"no compositions at {COMPOSITIONS.relative_to(ROOT)} — "
                 f"nothing to check, which is not the same as passing")
    if not RENDER_DIR.is_dir():
        sys.exit(f"no render fixtures at {RENDER_DIR.relative_to(ROOT)} — "
                 f"sequencer names can only be checked against a real render")

    # Rendered names per Composition, unioned over that Composition's fixtures.
    by_composition = {}
    parse_errors = []
    for fixture in sorted(RENDER_DIR.glob("*/composition.yaml")):
        try:
            docs = list(yaml.safe_load_all(fixture.read_text()))
        except yaml.YAMLError as exc:
            parse_errors.append((fixture.parent.name, f"cannot parse composition.yaml: {exc}"))
            continue
        name = next((composition_name(d) for d in docs if composition_name(d)), None)
        if not name:
            continue
        expected = fixture.parent / "expected.yaml"
        if not expected.is_file():
            continue
        names, err = rendered_names(expected, yaml)
        if err:
            parse_errors.append((fixture.parent.name, err))
            continue
        entry = by_composition.setdefault(name, {"names": set(), "fixtures": []})
        entry["names"] |= names
        entry["fixtures"].append(fixture.parent.name)

    problems = []
    checked_entries = 0
    checked_compositions = 0

    for path in sorted(COMPOSITIONS.glob("*.yaml")):
        try:
            docs = list(yaml.safe_load_all(path.read_text()))
        except yaml.YAMLError as exc:
            problems.append((path.name, "<file>", f"cannot parse: {exc}"))
            continue
        for doc in docs:
            name = composition_name(doc)
            if not name:
                continue
            rules = sequencer_rules(doc)
            if not rules:
                continue
            checked_compositions += 1
            known = by_composition.get(name)
            if not known:
                problems.append((
                    path.name, "<no fixture>",
                    f"Composition {name!r} declares sequencer rules but no render "
                    f"fixture renders it, so its names cannot be checked"))
                continue
            for step, entries in rules:
                for pos, item in enumerate(entries):
                    checked_entries += 1
                    ok, err = matches_any(item, known["names"])
                    if err:
                        problems.append((path.name, item, f"{step}: {err}"))
                    elif not ok:
                        after = entries[pos + 1:]
                        blocked = (", ".join(after) if after
                                   else "nothing — it is last in its sequence")
                        problems.append((
                            path.name, item,
                            f"{step}: matches no composed resource "
                            f"{name} renders. Held back: {blocked}"))

    print("")
    print("Sequencer targets — every ordered name is a resource that exists")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    for case, err in parse_errors:
        print(f"  {YELLOW}WARN{NC}  {case}: {err}")
    for where, item, why in problems:
        print(f"  {RED}ERROR{NC} {where}: {item}")
        print(f"        {DIM}{why}{NC}")
    print(f"{DIM}────────────────────────────────────────────────────────────{NC}")
    if problems:
        # "unsatisfiable name" and "could not be checked" want different
        # repairs, so the summary counts them apart rather than reporting a
        # missing fixture as a dead sequencer entry.
        unchecked = sum(1 for _, item, _ in problems if item == "<no fixture>")
        dead = len(problems) - unchecked
        if dead:
            print(f"{RED}{dead} sequencer name(s) nothing will ever satisfy.{NC}")
            print(f"{DIM}Everything ordered after such a name is never created, and the{NC}")
            print(f"{DIM}composite stays Synced=True while it waits. Delete the entry when{NC}")
            print(f"{DIM}its resource moved elsewhere; rename it when the resource did.{NC}")
        if unchecked:
            print(f"{RED}{unchecked} composition(s) ordering resources nothing renders in a "
                  f"fixture.{NC}")
            print(f"{DIM}Add a render fixture for it. Sequencer names are only checkable{NC}")
            print(f"{DIM}against a real render, and this is the failure that hides best.{NC}")
        return 1
    print(f"{GREEN}Every sequencer name matches a resource its Composition renders{NC} "
          f"({checked_entries} name(s) across {checked_compositions} composition(s)).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
