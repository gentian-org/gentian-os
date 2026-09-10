#!/usr/bin/env python3
# =============================================================================
# scripts/lint/lint-template-placeholders.py — shell placeholders Helm won't expand
# =============================================================================
# `${GENTIAN_OS_IMAGE_REPOSITORY}` sat in a Helm template for months. Helm does
# not expand shell syntax, nothing runs envsubst over that chart any more, and
# the string reached the cluster verbatim:
#
#   argocd-image-updater.argoproj.io/image-list: gentianos=${GENTIAN_OS_IMAGE_REPOSITORY}
#
# argocd-image-updater then looked for a repository with that literal name, found
# none, and skipped it — reporting `images_considered=2 images_skipped=1
# images_updated=0 errors=0` every two minutes, with a condition of *No errors*.
# The operator image was never updated on any cluster. Fixes were tested against
# binaries that did not contain them.
#
# Nothing could have caught it by reading the file: every other line in the same
# annotation block used `{{ .Values… }}`, and a placeholder in a template is
# indistinguishable from ordinary text. That is what this lint is for.
#
# It came from removing envsubst. Deleting the caller does not delete the
# placeholders the caller used to expand, and the leftovers are silent.
#
# Two tiers, by how certainly the answer is known:
#
#   ERROR  A `${VAR}` in a YAML *value* in a Helm template. Helm will not expand
#          it and no other expander runs over these files, so it is a literal.
#   WARN   A `${VAR}` inside a block scalar. Those are usually shell scripts in a
#          ConfigMap or a container command, where `${VAR}` is correct and
#          expanded at run time by the shell — reported, never fatal, because
#          calling those a defect would be a false accusation.
#
# Comments and CRD `description:` prose are skipped: both talk *about*
# placeholders, and neither is rendered.
#
# Usage:
#   scripts/lint/lint-template-placeholders.py [--strict]
#
#   --strict  treat warnings as failures
# =============================================================================

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# Only files Helm renders. A plain manifest applied with kubectl is not a
# template and has no expander to disappoint.
TEMPLATE_GLOBS = ("charts/**/templates/**/*.yaml", "kernel/**/templates/**/*.yaml")

PLACEHOLDER = re.compile(r"\$\{[A-Z][A-Z0-9_]*\}")

# `key: value` — a mapping entry with something after the colon.
MAPPING_VALUE = re.compile(r"^\s*[\w.\-/]+:\s*\S")

# A block scalar opener, as a mapping value (`key: |`) or a sequence item
# (`- |`). The first draft matched only the mapping form, so every shell script
# written as a container arg was reported as a defect.
BLOCK_OPEN = re.compile(r"^(\s*)(?:-\s+)?(?:[^:#]+:\s*)?[|>][-+]?\d*\s*$")

# A Helm action occupying its own line. These do not end a block scalar's body —
# a `{{- if }}` inside a script is written at whatever indentation reads best,
# frequently less than the block's, and treating that as the end of the block
# made everything after it look like YAML structure. They are also not YAML
# values themselves: `{{- $c := printf "--password=%s" "${VAR}" }}` builds a
# shell string, and the placeholder in it belongs to the shell.
HELM_ACTION = re.compile(r"^\s*\{\{.*\}\}\s*$")


def scan(path: Path) -> tuple[list[str], list[str]]:
    errors: list[str] = []
    warnings: list[str] = []
    block_indent: int | None = None

    for n, raw in enumerate(path.read_text().splitlines(), 1):
        line = raw.rstrip("\n")
        stripped = line.strip()
        if not stripped:
            continue
        indent = len(line) - len(line.lstrip())
        is_action = bool(HELM_ACTION.match(line))

        if block_indent is not None:
            # Only real YAML structure closes a block. A Helm action is not.
            if not is_action and indent <= block_indent:
                block_indent = None
            else:
                for m in PLACEHOLDER.finditer(line):
                    warnings.append(
                        f"{path.relative_to(ROOT)}:{n}: {m.group(0)} inside a block scalar "
                        f"— a shell expands this at run time."
                    )
                continue

        if BLOCK_OPEN.match(line):
            block_indent = indent
            continue

        if stripped.startswith("#") or is_action:
            continue

        if re.match(r"^\s*description:", line):
            continue

        # ERROR only for a plain mapping value — `key: …${VAR}…` — which is the
        # shape the real defect had, and the shape nothing expands.
        #
        # A sequence item is a warning instead. `- mariadb-admin -p"${PASS}"`
        # under `command:` is an argv entry a shell expands, and calling it a
        # defect would be a false accusation; distinguishing it properly means
        # knowing whether the enclosing key is `command`/`args`, which is more
        # YAML than a lint should reimplement. Reported, never fatal.
        if not MAPPING_VALUE.match(line):
            for m in PLACEHOLDER.finditer(line):
                warnings.append(
                    f"{path.relative_to(ROOT)}:{n}: {m.group(0)} in a sequence item "
                    f"— an argv entry a shell expands, or a literal if it is not."
                )
            continue

        for m in PLACEHOLDER.finditer(line):
            errors.append(
                f"{path.relative_to(ROOT)}:{n}: {m.group(0)} is a shell placeholder in a Helm "
                f"template.\n"
                f"    Helm renders this file and will not expand it, so the literal text reaches "
                f"the cluster.\n"
                f"    Use a chart value: {{{{ .Values.<name> }}}}"
            )
    return errors, warnings


def main() -> int:
    strict = "--strict" in sys.argv
    files: list[Path] = []
    for pattern in TEMPLATE_GLOBS:
        files.extend(sorted(ROOT.glob(pattern)))
    if not files:
        print("FATAL: no Helm templates matched — has the layout changed?", file=sys.stderr)
        return 1

    errors: list[str] = []
    warnings: list[str] = []
    for f in files:
        e, w = scan(f)
        errors.extend(e)
        warnings.extend(w)

    for w in warnings:
        print(f"\033[0;33mWARN\033[0m  {w}")
    for e in errors:
        print(f"\033[0;31mERROR\033[0m {e}", file=sys.stderr)

    if errors:
        print(f"\n{len(errors)} unexpandable placeholder(s) in {len(files)} templates.",
              file=sys.stderr)
        return 1
    if warnings and strict:
        print(f"\n{len(warnings)} warning(s), --strict.", file=sys.stderr)
        return 1
    print(f"\033[0;32mNo unexpandable placeholders\033[0m ({len(files)} templates scanned).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
