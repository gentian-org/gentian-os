#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-scaffold-schemas.py — does what we scaffold actually apply?
# =============================================================================
# The installer and gtnctl write manifests for people. A scaffold is the first
# thing a new cluster applies and the last thing anyone reads critically: it
# arrives already written, with comments explaining itself, so a field in it
# looks decided rather than guessed.
#
# Nothing checked that those fields exist. `gtnctl tenants deploy` scaffolded a
# tenant-defaults component carrying spec.mail.quotaPerUser and
# spec.mail.rateLimit, and TenantMail has neither -- it has mode, domain,
# smtpCredentialsSecret, dkimPublicKey, spfRecord and dmarcRecord. CRDs reject
# an unknown field outright, so EVERY first tenant deploy on a fresh cluster
# died at admission naming two fields no operator had ever read, and rolled the
# tenant back. Both had a plausible value and a comment above them. They had
# been enforcing nothing for as long as they had existed.
#
# The failure mode is what makes this worth a lint rather than a code review.
# A wrong scaffold does not fail where it is written; it fails on someone
# else's cluster, at admission, in a command that then rolls back and leaves no
# trace of which file was at fault.
#
# So: every manifest these generators emit is parsed out and checked, field by
# field, against the CRDs this repo ships. Same repo, same commit -- a scaffold
# and the schema it must satisfy travel together, and disagreeing is a build
# error rather than a support ticket.
#
# Scope, deliberately narrow. Only gentianos.io kinds, because those are the
# CRDs this repo owns and ships; a Kustomization or a Secret in the same
# heredoc is somebody else's schema and is skipped rather than guessed at.
# Only field NAMES, not values: a lint that type-checked would need the whole
# of OpenAPI and would start refusing things the API server accepts.
# =============================================================================

import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print("PyYAML not available; skipping scaffold schema lint.")
    sys.exit(0)

ROOT = Path(__file__).resolve().parents[2]
CRD_DIR = ROOT / "charts" / "gentian-os" / "crds"

# The generators. Each writes manifests a human is then expected to apply.
GENERATORS = [
    ROOT / "scripts" / "kubectl-gentian",
]

GREEN, YELLOW, RED, DIM, RESET = "\033[0;32m", "\033[1;33m", "\033[0;31m", "\033[2m", "\033[0m"


def load_crd_schemas():
    """kind -> spec schema, for every gentianos.io CRD this repo ships."""
    schemas = {}
    for path in sorted(CRD_DIR.glob("gentianos.io_*.yaml")):
        try:
            doc = yaml.safe_load(path.read_text())
        except Exception:
            continue
        if not doc or doc.get("kind") != "CustomResourceDefinition":
            continue
        kind = doc["spec"]["names"]["kind"]
        for version in doc["spec"].get("versions", []):
            props = (
                version.get("schema", {})
                .get("openAPIV3Schema", {})
                .get("properties", {})
                .get("spec")
            )
            if props:
                # Last version wins; the repo ships one served version per kind.
                schemas[(kind, version["name"])] = props
    return schemas


# Two heredocs belong to the same manifest when nothing but `cat` separates
# them. The generators build one document from several adjacent heredocs:
#
#     {
#       cat <<'DEOF'
#     apiVersion: gentianos.io/v1alpha1
#     ...
#     DEOF
#       cat <<'DEOF'
#       mail:
#     ...
#     DEOF
#     } > "${comp_dir}/defaults.yaml"
#
# so the bodies have to be rejoined before any of it parses. Joining ALL of
# them instead is what the first draft of this lint did, and it quietly
# appended the file's later python heredocs to the Tenant: the document then
# failed to parse, was skipped as "a fragment of shell", and the lint passed
# having checked nothing. A lint that cannot fail is worse than no lint, so
# the count of manifests checked is printed and a zero is an error below.
_ADJACENT = re.compile(r"^\s*cat\s*$")


def extract_manifest_groups(text):
    """Adjacent quoted-heredoc bodies, joined into one string per manifest.

    Quoted only ('TAG'), which is what the generators use for literal YAML --
    an unquoted heredoc interpolates, so its body is a template rather than a
    manifest and parsing it as YAML would be reading something that does not
    exist yet.
    """
    matches = list(re.finditer(r"<<-?'([A-Za-z_][A-Za-z0-9_]*)'\n(.*?)\n\1\n", text, re.S))
    groups, current, previous = [], [], None
    for match in matches:
        if previous is not None and not _ADJACENT.match(text[previous.end():match.start()]):
            groups.append("\n".join(current))
            current = []
        current.append(match.group(2))
        previous = match
    if current:
        groups.append("\n".join(current))
    return groups


def split_documents(blob):
    """Split concatenated heredoc bodies into YAML documents.

    The generators build one manifest from several adjacent heredocs, so the
    bodies are joined first and cut on apiVersion: -- the only line that
    reliably starts a document here. A document that does not parse is skipped
    rather than reported: these are fragments of shell, and a lint that failed
    on every one of them would be noise nobody reads.
    """
    docs, current = [], None
    for line in blob.splitlines():
        if line.startswith("apiVersion:"):
            if current is not None:
                docs.append("\n".join(current))
            current = [line]
        elif current is not None:
            current.append(line)
    if current is not None:
        docs.append("\n".join(current))
    return docs


def walk(node, schema, path, errors):
    """Compare a manifest's field names against the schema, depth-first."""
    if not isinstance(node, dict) or not isinstance(schema, dict):
        return
    # A schema that accepts anything ends the walk: the API server will not
    # reject a field here, so neither should this.
    if schema.get("x-kubernetes-preserve-unknown-fields") or "additionalProperties" in schema:
        return
    props = schema.get("properties")
    if not props:
        return
    for key, value in node.items():
        where = f"{path}.{key}" if path else key
        if key not in props:
            errors.append(where)
            continue
        child = props[key]
        if isinstance(value, dict):
            walk(value, child, where, errors)
        elif isinstance(value, list):
            items = child.get("items", {})
            for i, entry in enumerate(value):
                if isinstance(entry, dict):
                    walk(entry, items, f"{where}[{i}]", errors)


def main():
    schemas = load_crd_schemas()
    if not schemas:
        print(f"{YELLOW}No gentianos.io CRDs found under {CRD_DIR}; nothing to check.{RESET}")
        return 0

    print("Scaffold schemas — does every generated field exist on the CRD?")
    print(f"{DIM}{'─' * 60}{RESET}")

    failures, checked = [], 0
    for generator in GENERATORS:
        if not generator.exists():
            continue
        raws = []
        for group in extract_manifest_groups(generator.read_text()):
            raws.extend(split_documents(group))
        for raw in raws:
            try:
                doc = yaml.safe_load(raw)
            except Exception:
                continue
            if not isinstance(doc, dict):
                continue
            api = str(doc.get("apiVersion", ""))
            if not api.startswith("gentianos.io/"):
                continue
            kind, version = doc.get("kind"), api.split("/", 1)[1]
            schema = schemas.get((kind, version))
            if schema is None:
                print(f"  {YELLOW}WARN{RESET}  {generator.name}: {kind} {version} — no CRD shipped for it.")
                continue
            spec = doc.get("spec")
            if not isinstance(spec, dict):
                continue
            checked += 1
            errors = []
            walk(spec, schema, "spec", errors)
            for field in errors:
                failures.append((generator.name, kind, field))

    for name, kind, field in failures:
        print(f"  {RED}ERROR{RESET}  {name}: {kind} has no {field}")

    print(f"{DIM}{'─' * 60}{RESET}")
    if failures:
        print(f"{RED}{len(failures)} scaffolded field(s) the CRD would reject.{RESET}")
        print("  A CRD refuses an unknown field outright, so this scaffold fails")
        print("  at admission on someone else's cluster — not here.")
        return 1
    if checked == 0:
        print(f"{RED}No gentianos.io manifests were found in any generator.{RESET}")
        print("  The generators do scaffold them, so this means the extraction")
        print("  stopped matching — a passing lint here would be checking nothing.")
        return 1
    print(f"{GREEN}Every scaffolded field exists on the CRD it is applied against{RESET} "
          f"({checked} manifest(s) checked).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
