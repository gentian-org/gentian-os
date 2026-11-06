#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-version-pins.py - every component pin lives in versions.yaml
# =============================================================================
# versions.yaml is the one place a component's version is written: the
# installer, the bootstrap chart and CI read it from there, and an air-gapped
# mirror fetches exactly what it lists. A version written anywhere else is a
# second pin that drifts from the first and that the mirror never sees.
#
# Three rules, read from the repository alone:
#
#   1. Bootstrap chart (kernel/bootstrap/chart/templates): no `repoURL:` or
#      `targetRevision:` is a literal. An upstream chart's repository and
#      version come from `.Values.versions`, which B-01 fills from
#      versions.yaml; the platform's own sources come from the chart's values.
#
#   2. Every `.Values.versions.<component>.<field>` the bootstrap chart reads,
#      and every entry of its values.yaml `versions:` block, is a pin
#      versions.yaml has. A pin read but missing renders empty.
#
#   3. CI workflows, the Makefile and the installer's shell: no `*_VERSION`
#      variable is assigned a literal version. Read the pin with
#      scripts/lib/versions.sh instead. KNOWN lists the tools versions.yaml
#      does not hold, each with the reason.
# =============================================================================

import re
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
VERSIONS = ROOT / "versions.yaml"
CHART = ROOT / "kernel/bootstrap/chart"

# Literal versions that are not component pins, as (file, variable): tools the
# installer never pulls, which versions.yaml leaves out because it is the
# mirror inventory and no step could claim them.
KNOWN = {
    ("Makefile", "KUBECONFORM_VERSION"): "a CI and developer tool for lint-chart-schemas",
    ("scripts/tools/verify-authz-model.sh", "FGA_VERSION"): "a test tool; versions.yaml says why it is not there",
}

# Where rule 3 looks.
SHELL_PATHS = [".github/workflows", "Makefile", "install.sh", "scripts/lib", "scripts/steps",
               "scripts/bootstrap", "scripts/tools"]

LITERAL_FIELD = re.compile(r"^\s*(?:-\s+)?(repoURL|targetRevision):\s*(.*?)\s*$")
PIN_READ = re.compile(r"\.Values\.versions\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)"
                      r'|index\s+\.Values\.versions\s+"([^"]+)"\s+"([^"]+)"')
VERSION_ASSIGN = re.compile(r"\b([A-Z][A-Z0-9_]*_VERSION)\s*[?:]?=\s*['\"]?v?[0-9]+\.[0-9]+")


def tracked(*paths):
    out = subprocess.run(["git", "ls-files", "--", *paths], cwd=ROOT, capture_output=True, text=True, check=True)
    return [ROOT / p for p in out.stdout.splitlines() if p]


def main():
    pins = yaml.safe_load(VERSIONS.read_text(encoding="utf-8"))
    failures = []
    checked = 0

    def has_pin(component, field):
        return isinstance(pins.get(component), dict) and field in pins[component]

    # --- 1 and 2: the bootstrap chart ---------------------------------------
    templates = sorted((CHART / "templates").glob("*.yaml"))
    if not templates:
        failures.append(f"{CHART.relative_to(ROOT)}/templates holds no template; this lint reads the wrong place")
    for path in templates:
        rel = path.relative_to(ROOT)
        for n, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            if line.lstrip().startswith("#"):
                continue
            m = LITERAL_FIELD.match(line)
            if m:
                checked += 1
                if "{{" not in m.group(2):
                    failures.append(f"{rel}:{n}: {m.group(1)} is the literal {m.group(2)!r}; "
                                    "pin the chart in versions.yaml and read it from .Values.versions")
            for read in PIN_READ.finditer(line):
                component, field = (read.group(1), read.group(2)) if read.group(1) else (read.group(3), read.group(4))
                if not has_pin(component, field):
                    failures.append(f"{rel}:{n}: reads versions.{component}.{field}, which versions.yaml does not pin")

    defaults = (yaml.safe_load((CHART / "values.yaml").read_text(encoding="utf-8")) or {}).get("versions") or {}
    for component, fields in defaults.items():
        for field in fields or {}:
            if not has_pin(component, field):
                failures.append(f"{CHART.relative_to(ROOT)}/values.yaml: versions.{component}.{field} "
                                "is not a pin in versions.yaml")

    # --- 3: literal *_VERSION assignments ------------------------------------
    for path in tracked(*SHELL_PATHS):
        rel = str(path.relative_to(ROOT))
        try:
            text = path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, IsADirectoryError):
            continue
        for n, line in enumerate(text.splitlines(), start=1):
            if line.lstrip().startswith("#"):
                continue
            for m in VERSION_ASSIGN.finditer(line):
                checked += 1
                if (rel, m.group(1)) in KNOWN:
                    continue
                failures.append(f"{rel}:{n}: {m.group(1)} is a literal version; read it with "
                                "`bash scripts/lib/versions.sh <component> <field>`")

    known_seen = {(str(p.relative_to(ROOT)), m.group(1))
                  for p in tracked(*SHELL_PATHS) if p.is_file()
                  for m in VERSION_ASSIGN.finditer(p.read_text(encoding="utf-8", errors="replace"))}
    for entry in sorted(set(KNOWN) - known_seen):
        failures.append(f"KNOWN lists {entry[1]} in {entry[0]}, which no longer assigns it; drop the entry")

    print("\nVersion pins - every component version is in versions.yaml\n")
    if failures:
        for f in failures:
            print(f"  ✗ {f}")
        print(f"\n{len(failures)} version pin(s) outside versions.yaml.")
        return 1
    for (rel, var), why in sorted(KNOWN.items()):
        print(f"  ○ {var} in {rel}: {why}")
    print(f"\nAll {checked} repository and version fields read their pin from versions.yaml.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
