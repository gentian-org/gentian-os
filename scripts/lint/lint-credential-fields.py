#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-credential-fields.py — does everyone agree on the field name?
# =============================================================================
# Two cluster failures came from a credential stored under the right path and
# the wrong key. Both cost hours, because a value under the right path with the
# wrong field name reads as absent and presents as an ESO fault.
#
#   authz/openfga  holds `preshared_key`, written once and read under that name
#                  by OpenFGA's own ExternalSecret, while the operator chart
#                  asked OpenBao for `api-token`. The authz bridge called
#                  OpenFGA with no bearer token, no store id was published, and
#                  the portal never became Ready.
#
#   repositories/deployments  is written with `username` and `password` — by the
#                  seeder, and by the XRepository Composition that builds the
#                  ArgoCD repository Secret from the same two names — while the
#                  catalogue declared the second field as `token`. Its probe
#                  reported SecretSyncedError with the value sitting beside it
#                  under the other name.
#
# Once the consumer was wrong, once the catalogue was. So the check runs both
# ways, and it cannot stop at credentials.yaml: openfga is a path the catalogue
# does not describe at all.
#
# Two tiers, by how exactly the answer can be known:
#
#   ERROR  Catalogue against consumers. Both sides are YAML naming fields
#          explicitly, so a disagreement is a fact, not an inference.
#   WARN   Consumers against writers. Finding what a shell script writes means
#          reading heredocs and jq filters, and a writer this misses would
#          become a false accusation. Reported, never fatal.
#
# Usage:
#   scripts/lint/lint-credential-fields.py [--strict]
# =============================================================================

import os
import re
import sys
from collections import defaultdict

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
os.chdir(REPO)

RED, YELLOW, GREEN, NC = "\033[0;31m", "\033[1;33m", "\033[0;32m", "\033[0m"

errors = []
warnings = []

# The catalogue is the only generated carrier; comparing a consumer against it
# would compare the file against itself.
GENERATED = ("kernel/credentials/credential-requirements.yaml",)

# Fixture expectations describe a rendered tenant, not this cluster's kernel.
SKIP_DIRS = (".git", "node_modules", "testdata", "fixtures")


def yaml_files():
    for root, dirs, names in os.walk("."):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for n in names:
            if n.endswith((".yaml", ".yml", ".tmpl")):
                yield os.path.relpath(os.path.join(root, n), ".")


# ── What the catalogue declares ──────────────────────────────────────────────
#
# Parsed with regex rather than a YAML library: this runs in CI before anything
# is installed, and adding PyYAML as a lint dependency would make the check
# skippable on exactly the machines that most need it.
def declared_fields():
    out = {}
    try:
        src = open("credentials.yaml").read()
    except OSError:
        return out
    for block in re.split(r"\n  - name:", src)[1:]:
        m = re.search(r"vaultPath:\s*(\S+)", block)
        if not m:
            continue
        fields = re.findall(r"^\s+- key:\s*(\S+)", block, re.M)
        out[m.group(1)] = set(fields)
    return out


# ── What consumers read ──────────────────────────────────────────────────────
#
# An ExternalSecret names the path in remoteRef.key and the field in
# remoteRef.property, on adjacent lines. A property may be absent, which means
# the whole secret is pulled and no single field is being named.
def consumed_fields():
    out = defaultdict(lambda: defaultdict(set))
    key_re = re.compile(r"""key:\s*["']?(gentian-os/kernel/[^\s"'{}]+)""")
    prop_re = re.compile(r"""property:\s*["']?([A-Za-z0-9_.\-]+)""")
    for path in yaml_files():
        if path.replace(os.sep, "/") in GENERATED:
            continue
        try:
            lines = open(path, errors="replace").read().split("\n")
        except OSError:
            continue
        for i, line in enumerate(lines):
            m = key_re.search(line)
            if not m:
                continue
            for j in range(i, min(i + 4, len(lines))):
                p = prop_re.search(lines[j])
                if p:
                    out[m.group(1)][p.group(1)].add(path)
                    break
    return out


# ── What consumers read through Helm values ──────────────────────────────────
#
# The chart does not write the pair literally. It renders
#   key:      {{ .Values.<dotted>.openbaoPath | quote }}
#   property: {{ .Values.<dotted>.openbaoProperty | default "preshared_key" | quote }}
# so a scan for literals sees neither, and the openfga failure — the whole
# reason this lint exists — would pass unnoticed. The dotted reference is
# resolved against the chart's values.yaml, falling back to the literal inside
# `default` when values.yaml leaves it unset, which is where the wrong name
# lived.
def _dotted_scalars(path):
    """Every scalar leaf in a values file, keyed by its dotted path."""
    out, stack = {}, []
    try:
        lines = open(path, errors="replace").read().split("\n")
    except OSError:
        return out
    for raw in lines:
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        m = re.match(r"^(\s*)([A-Za-z0-9_.\-]+):\s*(.*)$", raw)
        if not m:
            continue
        indent, key, value = len(m.group(1)), m.group(2), m.group(3).strip()
        while stack and stack[-1][0] >= indent:
            stack.pop()
        dotted = ".".join([k for _, k in stack] + [key])
        if value and not value.startswith("#"):
            out[dotted] = value.strip("\"'")
        else:
            stack.append((indent, key))
    return out


def helm_openbao_refs():
    out = defaultdict(lambda: defaultdict(set))
    ref_re = re.compile(r"\.Values\.([A-Za-z0-9_.]+?)\.openbao(Path|Property)")
    default_re = re.compile(r"""default\s+["']([A-Za-z0-9_.\-]+)["']""")
    for chart in sorted(os.listdir("charts")) if os.path.isdir("charts") else []:
        values_file = os.path.join("charts", chart, "values.yaml")
        values = _dotted_scalars(values_file)
        tmpl_dir = os.path.join("charts", chart, "templates")
        if not os.path.isdir(tmpl_dir):
            continue
        for name in sorted(os.listdir(tmpl_dir)):
            tmpl = os.path.join(tmpl_dir, name)
            try:
                lines = open(tmpl, errors="replace").read().split("\n")
            except OSError:
                continue
            for i, line in enumerate(lines):
                if not re.match(r"\s*property:", line):
                    continue
                pm = ref_re.search(line)
                if not pm or pm.group(2) != "Property":
                    continue
                prop = values.get(pm.group(1) + ".openbaoProperty")
                if not prop:
                    dm = default_re.search(line)
                    prop = dm.group(1) if dm else None
                if not prop:
                    continue
                # The path is on a nearby key: line, by the same reference.
                for j in range(max(0, i - 3), i + 1):
                    km = ref_re.search(lines[j])
                    if km and km.group(2) == "Path":
                        vault = values.get(km.group(1) + ".openbaoPath")
                        if vault:
                            out[vault][prop].add(tmpl)
                        break
    return out


# ── What the installer writes ────────────────────────────────────────────────
#
# kv_put/kv_put_once take a path and a JSON document built by a heredoc or by
# jq, plus a few direct `bao kv put path field=value` calls. Best-effort by
# construction, which is why nothing here fails the build.
def written_fields():
    out = defaultdict(set)
    sources = ["scripts/bootstrap/seed-openbao.sh"]
    sources += [
        os.path.join("scripts/lib", f)
        for f in sorted(os.listdir("scripts/lib"))
        if f.endswith(".sh")
    ]
    for src_path in sources:
        try:
            src = open(src_path, errors="replace").read()
        except OSError:
            continue
        for m in re.finditer(
            r'kv_put(?:_once)?\s+"([^"]+)"\s+"\$\((.*?)\)"', src, re.S
        ):
            path = m.group(1)
            if not path.startswith("gentian-os/"):
                path = "gentian-os/kernel/" + path
            out[path] |= set(
                re.findall(r'[{,\n]\s*"?([a-z0-9][a-z0-9_.\-]*)"?\s*:', m.group(2))
            )
        for m in re.finditer(
            r'bao kv put[^\n]*?"?(gentian-os/kernel/[^\s"]+)"?((?:[^\n]*\\\n)*[^\n]*)',
            src,
        ):
            out[m.group(1)] |= set(
                re.findall(r"\b([a-z0-9][a-z0-9_.\-]*)=", m.group(2))
            )
    return out


declared = declared_fields()
consumed = consumed_fields()
for _p, _fields in helm_openbao_refs().items():
    for _f, _where in _fields.items():
        consumed[_p][_f] |= _where
written = written_fields()

print("")
print("Credential field lint — does every reader name a field something writes?")
print("")

for vault_path in sorted(consumed):
    for field, files in sorted(consumed[vault_path].items()):
        where = ", ".join(sorted(files)[:2])

        # ERROR: the catalogue describes this path and does not list this field.
        if vault_path in declared and declared[vault_path]:
            if field not in declared[vault_path]:
                errors.append(
                    f"{where}: reads '{field}' from {vault_path}, which the "
                    f"catalogue declares as {sorted(declared[vault_path])}. "
                    f"A value under the right path with the wrong key reads as absent."
                )
            continue

        # WARN: nothing found that writes this field at this path.
        if vault_path in written and written[vault_path]:
            if field not in written[vault_path]:
                warnings.append(
                    f"{where}: reads '{field}' from {vault_path}, but the "
                    f"installer writes {sorted(written[vault_path])} there."
                )

# A field the catalogue declares that the installer never writes at that path is
# the other direction of the same bug: repositories/deployments was written with
# `username` and `password` while the catalogue called the second one `token`,
# and the probe reported the credential missing with the value sitting beside it.
#
# Only for paths where a writer was actually found — an empty writer set means
# the extraction did not understand that call, not that nothing writes there.
for vault_path, fields in sorted(declared.items()):
    if not written.get(vault_path):
        continue
    for field in sorted(fields - written[vault_path]):
        warnings.append(
            f"credentials.yaml: declares '{field}' at {vault_path}, but the "
            f"installer writes {sorted(written[vault_path])} there."
        )

for e in errors:
    print(f"  {RED}ERROR{NC}  {e}")
for w in warnings:
    print(f"  {YELLOW}WARN{NC}   {w}")

print("")
if errors:
    print(f"{RED}{len(errors)} error(s), {len(warnings)} warning(s).{NC}")
    sys.exit(1)
if warnings:
    print(f"{YELLOW}0 errors, {len(warnings)} warning(s).{NC}")
    sys.exit(1 if "--strict" in sys.argv else 0)
print(f"{GREEN}Every credential field name agrees across catalogue, readers and writers.{NC}")
