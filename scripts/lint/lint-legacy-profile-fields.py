#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-legacy-profile-fields.py - a renamed field is pruned, not
# refused
# =============================================================================
# ComponentProfile's vocabulary changed: tenancy became classes,
# requires.contracts became requires.services, package.apiIntegration became
# package.api, package.compositionRef became package.composition,
# customization.addon became package.addon, and package.deploymentMethod is
# gone because the package union already says what it is.
#
# None of that can be caught by the CRD. A structural schema PRUNES an unknown
# field before CEL runs, so a profile that still says `requires.contracts` is
# admitted, its requirements are silently dropped, and the component installs
# without the database it asked for. Failing that way is worse than failing
# loudly: the object is Ready and wrong.
#
# So the check lives here. It reads every YAML document that is a
# ComponentProfile and refuses the old spellings by name, with the new one
# beside each. AppProfile is untouched: kernelRequirements, deploymentMethod,
# compositionRef and customization.addon are still that kind's own vocabulary,
# and stay until the catalogue converts.
# =============================================================================

import shutil
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the CI image has it
    print("PyYAML is required for this check", file=sys.stderr)
    sys.exit(2)

ROOT = Path(__file__).resolve().parents[2]

# The chart's profiles are Helm templates and do not parse as YAML, so they
# are rendered rather than read. What ships is what is checked.
CHART = "charts/gentian-os"
CHART_VALUES = [
    "desktop.enabled=true",
    "adminConsole.enabled=true",
]

# Plain YAML that may also hold a ComponentProfile.
SEARCH = [
    "crossplane",
    "kernel",
    "config/samples",
]

# old path -> what to write instead
RENAMED = {
    ("spec", "tenancy"): "spec.classes, with service/app/shared-app",
    ("spec", "requires", "contracts"): "spec.requires.services",
    ("spec", "requires", "kernelRequirements"): "spec.requires.services",
    ("spec", "kernelRequirements"): "spec.requires.services",
    ("spec", "package", "apiIntegration"): "spec.package.api",
    ("spec", "package", "compositionRef"): "spec.package.composition",
    ("spec", "package", "deploymentMethod"): "nothing: the package union says what it is",
    ("spec", "customization", "addon"): "spec.package.addon",
}


def dig(doc, path):
    """Walk a dotted path, returning True when every step exists."""
    node = doc
    for key in path:
        if not isinstance(node, dict) or key not in node:
            return False
        node = node[key]
    return True


def documents(path):
    """Parse a plain YAML file, reporting whether it parsed at all."""
    try:
        return list(yaml.safe_load_all(path.read_text(encoding="utf-8"))), True
    except yaml.YAMLError:
        return [], False


def rendered_chart():
    """helm template the operator chart, so the shipped profiles are checked
    as they are installed rather than as they are written."""
    helm = shutil.which("helm")
    if helm is None:
        return None, "helm is not on PATH"
    cmd = [helm, "template", "lint", str(ROOT / CHART)]
    for v in CHART_VALUES:
        cmd += ["--set", v]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        return None, out.stderr.strip().splitlines()[-1] if out.stderr else "helm failed"
    try:
        return list(yaml.safe_load_all(out.stdout)), None
    except yaml.YAMLError as err:
        return None, str(err)


def scan(doc, origin, findings):
    if not isinstance(doc, dict) or doc.get("kind") != "ComponentProfile":
        return 0
    name = (doc.get("metadata") or {}).get("name", "<unnamed>")
    for old, new in RENAMED.items():
        if dig(doc, old):
            findings.append((origin, name, ".".join(old), new))
    return 1


def main():
    findings = []
    unparsed = []
    checked = 0

    docs, err = rendered_chart()
    if err is not None:
        print(f"{CHART}: could not be rendered: {err}")
        unparsed.append(CHART)
    else:
        for doc in docs:
            checked += scan(doc, CHART, findings)

    for base in SEARCH:
        root = ROOT / base
        if not root.is_dir():
            continue
        for path in sorted(root.rglob("*.yaml")):
            docs, parsed = documents(path)
            if not parsed:
                if "kind: ComponentProfile" in path.read_text(encoding="utf-8"):
                    unparsed.append(path.relative_to(ROOT))
                continue
            for doc in docs:
                checked += scan(doc, path.relative_to(ROOT), findings)

    for path, name, old, new in findings:
        print(f"{path}: ComponentProfile/{name} still says {old}")
        print(f"    write {new}")
    for path in unparsed:
        print(f"{path}: holds a ComponentProfile this check could not parse")

    if findings or unparsed:
        print()
        print(
            f"{len(findings) + len(unparsed)} legacy field(s). A renamed field is "
            "pruned by the API server, not refused, so the component installs "
            "without what it asked for."
        )
        return 1

    print(f"No legacy fields. {checked} ComponentProfile(s) checked.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
