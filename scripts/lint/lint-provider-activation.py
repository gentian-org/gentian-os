#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-provider-activation.py — a resource type nobody switched on
# =============================================================================
# Crossplane is installed without its default "activate everything" policy
# (scripts/steps/A-04-crossplane.sh). A provider that declares safe-start then
# ships its resource types as ManagedResourceDefinitions, and only the ones
# crossplane/providers/activation.yaml names become CustomResourceDefinitions
# with a running controller. That is the point: the vault and keycloak
# providers ship several hundred types between them and the platform uses a
# few dozen, and on a managed control plane every type costs memory.
#
# The price is a new way to fail quietly. A Composition that grows a resource
# of a type the list does not name renders, passes `crossplane render`, passes
# its fixture -- and on a cluster the API server has no such kind, so the
# resource never appears and the composite waits.
#
# This is the check that turns that into a red build. It collects every
# provider resource type the platform uses:
#
#   YAML and templates   every literal `apiVersion: <provider group>/<version>`
#                        with the `kind:` beside it. Text, not parsed YAML: the
#                        Compositions are go-templating text. The render
#                        fixtures' expected outputs are read the same way and
#                        are what is actually composed, so a kind that sits
#                        behind a conditional is seen when a fixture renders it.
#   Go                   schema.GroupVersionKind literals, apiVersion strings
#                        and +kubebuilder:rbac markers that name a provider
#                        group.
#   shell and Python     `kubectl get <resource>.<group>` style names.
#
# and fails when
#
#   1. a type in use is not activated;
#   2. a use cannot be resolved to a type (a templated kind, a kind the pinned
#      package does not ship) -- silence there would be the gap itself;
#   3. the activation list names a type the pinned provider package does not
#      ship (a typo activates nothing, and nothing says so);
#   4. the pinned type lists are not the ones for the versions providers.yaml
#      installs.
#
# A kind is turned into its plural by reading crossplane/providers/types/,
# which is fetched from the provider packages at the pinned versions
# (`make refresh-provider-types`). Nothing here guesses a plural.
#
# What it cannot see: a type that appears only behind a template variable
# (`apiVersion: {{ $group }}/v1`) and in no fixture, and a type a resource
# reaches only through a reference (`...Ref`/`...Selector`) to a kind nothing
# composes. Keep the fixtures covering what the Compositions can produce.
#
# Usage:
#   scripts/lint/lint-provider-activation.py
#       this repository, and the gentian-apps checkout beside it when there
#       is one (GENTIAN_APPS_DIR, default ../gentian-apps).
#
#   scripts/lint/lint-provider-activation.py --tree <dir> [--tree <dir> ...]
#       only the given trees, against this repository's activation list. This
#       is the form another repository's CI calls for its own compositions:
#           python3 gentian-os/scripts/lint/lint-provider-activation.py --tree .
#
#   --activation <file>  --types <dir>  --providers <file>
#       other inputs than this repository's; the lint's own tests use them.
# =============================================================================

import argparse
import fnmatch
import os
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]

SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "bin", "__pycache__", ".venv", "dist"}
# Relative to the scanned tree. Prose, the pinned data and the lint itself name
# types without using them; a CRD definition is not a use of the type either.
SKIP_PREFIXES = (
    "docs/",
    "crossplane/providers/types/",
    "crossplane/providers/activation.yaml",
    "scripts/lint/lint-provider-activation.py",
    "scripts/gen/gen-provider-types.py",
    "scripts/tests/test-provider-activation",
    "config/crd/",
)
YAML_SUFFIXES = {".yaml", ".yml", ".tpl", ".tmpl", ".gotmpl"}
SCRIPT_SUFFIXES = {".sh", ".py", ""}

API_VERSION = re.compile(r"""^(\s*(?:-\s+)?)["']?apiVersion["']?:\s*["']?([A-Za-z0-9.-]+)/(v[A-Za-z0-9]+)""")
KIND = re.compile(r"""^(\s*(?:-\s+)?)["']?kind["']?:\s*["']?([^\s"'#]+)""")
DIRECTIVE_ONLY = re.compile(r"^\s*\{\{.*\}\}\s*$")
TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9.-]*\.[a-z]{2,}")
GO_GROUP = re.compile(r"""Group:\s*"([a-z0-9.-]+)\"""")
GO_KIND = re.compile(r"""(?:Kind:\s*|"kind":\s*)"([A-Za-z0-9]+)\"""")
GO_API_VERSION = re.compile(r""""([a-z0-9.-]+)/v[a-z0-9]+\"""")
GO_MARKER = re.compile(r"\+kubebuilder:rbac:.*?groups=([^,\s]+),resources=([^,\s]+)")


class Types:
    """The pinned provider packages' types."""

    def __init__(self, types_dir: Path):
        self.by_name: dict[str, dict] = {}
        self.by_group_kind: dict[tuple[str, str], dict] = {}
        self.by_group_word: dict[tuple[str, str], dict] = {}
        self.groups: set[str] = set()
        self.packages: dict[str, str] = {}
        files = sorted(types_dir.glob("*.yaml"))
        if not files:
            sys.exit(f"FATAL: no provider type lists in {types_dir} -- run `make refresh-provider-types`.")
        for f in files:
            data = yaml.safe_load(f.read_text())
            self.packages[data["provider"]] = data["package"]
            for t in data["types"]:
                plural, group = t["name"].split(".", 1)
                entry = dict(t, provider=data["provider"], group=group, plural=plural,
                             safeStart=bool(data["safeStart"]))
                self.by_name[t["name"]] = entry
                self.by_group_kind[(group, t["kind"])] = entry
                self.by_group_word[(group, plural)] = entry
                self.by_group_word[(group, t["singular"])] = entry
                self.groups.add(group)

    def needs_activation(self, entry: dict) -> bool:
        return entry["safeStart"] and entry["managed"]


class Findings:
    def __init__(self):
        self.used: dict[str, list[str]] = {}
        self.errors: list[str] = []

    def use(self, entry: dict, where: str):
        self.used.setdefault(entry["name"], []).append(where)

    def error(self, where: str, message: str):
        self.errors.append(f"{where}: {message}")


def _key_column(prefix: str) -> int:
    return len(prefix)


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def _is_noise(line: str) -> bool:
    stripped = line.strip()
    return not stripped or stripped.startswith("#") or bool(DIRECTIVE_ONLY.match(line))


def _kind_beside(lines: list[str], index: int, column: int) -> str | None:
    """The value of the `kind:` key in the same mapping as lines[index]."""
    for step in (1, -1):
        i = index + step
        while 0 <= i < len(lines):
            line = lines[i]
            if _is_noise(line):
                i += step
                continue
            m = KIND.match(line)
            if m and _key_column(m.group(1)) == column:
                return m.group(2)
            if _indent(line) < column and not (step == -1 and m):
                # Left the mapping. Going backwards, the mapping's first key
                # may sit behind a list dash, which the column test above
                # already accepted when it was `kind`.
                break
            if step == -1 and line.lstrip(" ").startswith("- ") and _indent(line) + 2 == column:
                break
            i += step
    return None


def scan_yaml(rel: str, text: str, types: Types, out: Findings):
    lines = text.splitlines()
    for i, line in enumerate(lines):
        m = API_VERSION.match(line)
        if not m:
            continue
        group = m.group(2)
        if group not in types.groups:
            continue
        where = f"{rel}:{i + 1}"
        kind = _kind_beside(lines, i, _key_column(m.group(1)))
        if kind is None:
            out.error(where, f"apiVersion {group}/{m.group(3)} has no `kind:` beside it; "
                             "cannot tell which resource type this is.")
            continue
        if "{{" in kind or "$" in kind:
            out.error(where, f"the kind beside apiVersion {group}/{m.group(3)} is templated ({kind}); "
                             "write it out so the type can be checked against the activation list.")
            continue
        entry = types.by_group_kind.get((group, kind))
        if entry is None:
            out.error(where, f"{kind}.{group} is not a type the pinned provider package ships "
                             "(see crossplane/providers/types/).")
            continue
        out.use(entry, where)


def scan_tokens(rel: str, text: str, types: Types, out: Findings):
    """`kubectl get client.openidclient.keycloak.crossplane.io` and the like."""
    for i, line in enumerate(text.splitlines()):
        for m in TOKEN.finditer(line):
            token = m.group(0).lower()
            if token in types.groups or "." not in token:
                continue
            word, group = token.split(".", 1)
            if group not in types.groups:
                continue
            where = f"{rel}:{i + 1}"
            entry = types.by_group_word.get((group, word))
            if entry is None:
                out.error(where, f"{token}: the provider group {group} has no resource named {word!r} "
                                 "at the pinned version.")
                continue
            out.use(entry, where)


def scan_go(rel: str, text: str, types: Types, out: Findings):
    lines = text.splitlines()
    for i, line in enumerate(lines):
        where = f"{rel}:{i + 1}"
        marker = GO_MARKER.search(line)
        if marker:
            for group in marker.group(1).split(";"):
                if group not in types.groups:
                    continue
                for resource in marker.group(2).split(";"):
                    resource = resource.split("/", 1)[0]
                    entry = types.by_group_word.get((group, resource))
                    if entry is None:
                        out.error(where, f"{resource}.{group} is not a type the pinned provider package ships.")
                    else:
                        out.use(entry, where)
            continue
        if line.lstrip().startswith("//"):
            continue
        groups = [g for g in GO_GROUP.findall(line) if g in types.groups]
        groups += [g for g in GO_API_VERSION.findall(line) if g in types.groups]
        for group in groups:
            # The kind of a GroupVersionKind or an unstructured object sits on
            # the same line or within the next few.
            kind = None
            for near in lines[i:i + 4]:
                k = GO_KIND.search(near)
                if k:
                    kind = k.group(1)
                    break
            if kind is None:
                out.error(where, f"provider group {group} is named without a kind nearby; "
                                 "cannot tell which resource type this is.")
                continue
            entry = types.by_group_kind.get((group, kind))
            if entry is None:
                out.error(where, f"{kind}.{group} is not a type the pinned provider package ships.")
                continue
            out.use(entry, where)


def scan_tree(tree: Path, label: str, types: Types, out: Findings) -> int:
    count = 0
    for dirpath, dirnames, filenames in os.walk(tree):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for name in sorted(filenames):
            path = Path(dirpath) / name
            rel = path.relative_to(tree).as_posix()
            if rel.startswith(SKIP_PREFIXES):
                continue
            suffix = path.suffix
            if suffix not in YAML_SUFFIXES and suffix != ".go" and suffix not in SCRIPT_SUFFIXES:
                continue
            if suffix == "" and not rel.startswith("scripts/"):
                continue
            try:
                text = path.read_text()
            except (UnicodeDecodeError, OSError):
                continue
            shown = f"{label}{rel}"
            count += 1
            if suffix in YAML_SUFFIXES:
                scan_yaml(shown, text, types, out)
                scan_tokens(shown, text, types, out)
            elif suffix == ".go":
                scan_go(shown, text, types, out)
            else:
                scan_tokens(shown, text, types, out)
    return count


def load_activation(path: Path) -> list[str]:
    entries: list[str] = []
    found = False
    for doc in yaml.safe_load_all(path.read_text()):
        if not isinstance(doc, dict):
            continue
        if doc.get("kind") != "ManagedResourceActivationPolicy":
            sys.exit(f"FATAL: {path} holds a {doc.get('kind')}; only ManagedResourceActivationPolicy belongs there.")
        found = True
        activate = (doc.get("spec") or {}).get("activate")
        if not isinstance(activate, list) or not activate:
            sys.exit(f"FATAL: {path}: {doc.get('metadata', {}).get('name')} has no spec.activate entries.")
        entries.extend(str(a) for a in activate)
    if not found:
        sys.exit(f"FATAL: {path} holds no ManagedResourceActivationPolicy.")
    return entries


def check_pins(providers_file: Path, types: Types, errors: list[str]):
    installed = {}
    for doc in yaml.safe_load_all(providers_file.read_text()):
        if isinstance(doc, dict) and doc.get("kind") == "Provider":
            installed[doc["metadata"]["name"]] = doc["spec"]["package"]
    for name, package in sorted(installed.items()):
        pinned = types.packages.get(name)
        if pinned is None:
            errors.append(f"{providers_file.name} installs {name} but crossplane/providers/types/ has no type "
                          "list for it. Run `make refresh-provider-types`.")
        elif pinned != package:
            errors.append(f"{name}: providers.yaml installs {package}, the type list was read from {pinned}. "
                          "Run `make refresh-provider-types`.")
    for name in sorted(set(types.packages) - set(installed)):
        errors.append(f"crossplane/providers/types/{name}.yaml describes a provider providers.yaml does not "
                      "install. Run `make refresh-provider-types`.")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tree", action="append", default=[], type=Path)
    parser.add_argument("--activation", type=Path, default=ROOT / "crossplane/providers/activation.yaml")
    parser.add_argument("--types", type=Path, default=ROOT / "crossplane/providers/types")
    parser.add_argument("--providers", type=Path, default=ROOT / "crossplane/providers/providers.yaml")
    args = parser.parse_args()

    types = Types(args.types)
    activation = load_activation(args.activation)
    out = Findings()
    errors: list[str] = []

    check_pins(args.providers, types, errors)

    # 3. every activation entry names something the pinned packages ship.
    for entry in activation:
        if entry != entry.strip() or not entry:
            errors.append(f"activation entry {entry!r} is empty or carries whitespace.")
            continue
        matches = [n for n in types.by_name if fnmatch.fnmatchcase(n, entry)]
        if not matches:
            errors.append(f"activation entry {entry} matches no type of any pinned provider package; "
                          "it activates nothing. Check the spelling against crossplane/providers/types/.")
    if len(set(activation)) != len(activation):
        dupes = sorted({a for a in activation if activation.count(a) > 1})
        errors.append(f"activation entries listed twice: {', '.join(dupes)}")

    def activated(name: str) -> bool:
        return any(fnmatch.fnmatchcase(name, a) for a in activation)

    trees: list[tuple[Path, str]] = []
    if args.tree:
        trees = [(t.resolve(), f"{t}/" if len(args.tree) > 1 else "") for t in args.tree]
    else:
        trees = [(ROOT, "")]
        apps = Path(os.environ.get("GENTIAN_APPS_DIR", ROOT.parent / "gentian-apps"))
        if apps.is_dir():
            trees.append((apps.resolve(), "gentian-apps/"))
    scanned = 0
    for tree, label in trees:
        if not tree.is_dir():
            sys.exit(f"FATAL: {tree} is not a directory.")
        scanned += scan_tree(tree, label, types, out)

    errors.extend(out.errors)

    # 1. every type in use is activated.
    needed = {n for n in out.used if types.needs_activation(types.by_name[n])}
    for name in sorted(needed):
        if activated(name):
            continue
        places = out.used[name]
        shown = ", ".join(places[:3]) + (f" and {len(places) - 3} more" if len(places) > 3 else "")
        errors.append(
            f"{name} ({types.by_name[name]['kind']}, {types.by_name[name]['provider']}) is used at {shown} "
            f"but is not in {args.activation.name}.\n"
            f"       On a cluster the kind would not exist and the resource would never appear. "
            f"Add `- {name}` to spec.activate."
        )

    if not args.tree and not needed:
        errors.append("no provider resource type was found in use at all -- has the layout changed?")

    if errors:
        for e in errors:
            print(f"ERROR: {e}", file=sys.stderr)
        print(f"\n{len(errors)} problem(s) with the provider type activation list.", file=sys.stderr)
        return 1

    idle = sorted(
        n for n, t in types.by_name.items()
        if types.needs_activation(t) and activated(n) and n not in needed
    )
    if idle and not args.tree:
        print("note: activated but not found in use (harmless; remove when certain): " + ", ".join(idle))
    total = sum(1 for t in types.by_name.values() if types.needs_activation(t))
    print(f"Every provider resource type in use is activated "
          f"({len(needed)} in use, {scanned} files read; "
          f"{total} types shipped by the safe-start providers stay off unless listed).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
