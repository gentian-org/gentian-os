#!/usr/bin/env python3
"""The documentation lint: links resolve, anchors exist, every page under
docs/ carries its front matter and is listed in docs/index.md, and prose is
written in American English.

Prose is what is left of a Markdown file after fenced code blocks, inline
code, link targets and HTML comments are removed: identifiers keep their
spelling, sentences do not.
"""
from __future__ import annotations

import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
os.chdir(ROOT)

AUDIENCES = {"customer", "operator", "app-author", "architect", "auditor", "store-vendor", "contributor"}
TYPES = {"overview", "tutorial", "guide", "reference", "concept", "decision", "release", "contributing"}
BRITISH = {
    "catalogue": "catalog", "catalogues": "catalogs", "organisation": "organization",
    "organisations": "organizations", "licence": "license", "licences": "licenses", "colour": "color",
    "colours": "colors", "centre": "center", "analyse": "analyze", "authorise": "authorize",
    "authorised": "authorized", "authorisation": "authorization", "behaviour": "behavior",
    "behaviours": "behaviors", "customise": "customize", "customised": "customized",
    "initialise": "initialize", "initialised": "initialized", "optimise": "optimize",
    "normalise": "normalize", "serialise": "serialize", "serialised": "serialized", "favour": "favor",
    "labelled": "labeled", "modelled": "modeled", "cancelled": "canceled", "artefact": "artifact",
    "artefacts": "artifacts", "fulfil": "fulfill", "fulfilment": "fulfillment", "grey": "gray",
    "programme": "program", "synchronise": "synchronize", "recognise": "recognize", "utilise": "use",
}
BRITISH_RE = re.compile(r"\b(" + "|".join(BRITISH) + r")\b", re.I)
LINK_RE = re.compile(r"\[([^\]\n]*)\]\(([^)\s]+)\)")
FM_RE = re.compile(r"\A---\n(.*?)\n---\n", re.S)

failures: list[str] = []


def fail(msg: str) -> None:
    failures.append(msg)


VENDORED = ("/manifests/charts/", "charts/infra/")  # upstream charts keep their own READMEs


def tracked_md() -> list[str]:
    out = subprocess.run(["git", "ls-files", "*.md", "**/*.md"], capture_output=True, text=True, check=True).stdout
    return sorted(f for f in set(out.split()) if not any(v in f for v in VENDORED))


def slug(heading: str) -> str:
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", heading)  # link text only
    text = re.sub(r"[`*_]", "", text).strip().lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


_anchors: dict[str, set[str]] = {}


def anchors(path: str) -> set[str]:
    if path in _anchors:
        return _anchors[path]
    found: set[str] = set()
    seen: dict[str, int] = {}
    in_code = False
    for line in open(path, encoding="utf-8"):
        if line.startswith("```") or line.startswith("~~~"):
            in_code = not in_code
            continue
        if in_code:
            continue
        m = re.match(r"#{1,6}\s+(.*)", line)
        if m:
            s = slug(m.group(1))
            n = seen.get(s, 0)
            seen[s] = n + 1
            found.add(s if n == 0 else f"{s}-{n}")
        for a in re.findall(r'<a\s+(?:name|id)="([^"]+)"', line):
            found.add(a)
    _anchors[path] = found
    return found


def prose(text: str) -> list[tuple[int, str]]:
    """(line number, prose) pairs with code, link targets and comments removed."""
    text = re.sub(r"<!--.*?-->", lambda m: "\n" * m.group(0).count("\n"), text, flags=re.S)
    text = re.sub(r"`[^`]*`", lambda m: "\n" * m.group(0).count("\n"), text)  # inline code, even across lines
    out: list[tuple[int, str]] = []
    in_code = False
    for n, line in enumerate(text.split("\n"), 1):
        if line.lstrip().startswith(("```", "~~~")):
            in_code = not in_code
            continue
        if in_code or line.startswith("    "):
            continue
        line = re.sub(r"\]\([^)]*\)", "]", line)
        line = re.sub(r"\\?<[\w./-]+>", "", line)  # a <placeholder> is an identifier
        line = re.sub(r"\b[\w-]+(?:\.[\w-]+)+\b", "", line)  # so is a dotted path (spec.catalogue.storeUrl)
        line = re.sub(r"https?://\S+", "", line)
        out.append((n, line))
    return out


def check_file(path: str) -> None:
    text = open(path, encoding="utf-8").read()
    base = os.path.dirname(path)
    in_code = False
    for n, line in enumerate(text.split("\n"), 1):
        if line.lstrip().startswith(("```", "~~~")):
            in_code = not in_code
            continue
        if in_code:
            continue
        for _, url in LINK_RE.findall(line):
            if url.startswith(("http://", "https://", "mailto:")) or url.startswith("<"):
                continue
            target, _, anchor = url.partition("#")
            if "{" in target or "<" in target:  # a placeholder, not a path
                continue
            resolved = os.path.normpath(os.path.join(base, target)) if target else path
            if not os.path.exists(resolved):
                fail(f"{path}:{n}: link target does not exist: {url}")
                continue
            if anchor and resolved.endswith(".md") and anchor.lower() not in anchors(resolved):
                fail(f"{path}:{n}: no heading for anchor #{anchor} in {os.path.relpath(resolved)}")
    for n, line in prose(text):
        for m in BRITISH_RE.finditer(line):
            word = m.group(1)
            fail(f"{path}:{n}: British spelling \"{word}\"; write \"{BRITISH[word.lower()]}\"")


def check_front_matter(path: str) -> None:
    text = open(path, encoding="utf-8").read()
    m = FM_RE.match(text)
    if not m:
        fail(f"{path}: no front matter (audience, type)")
        return
    fields = dict(re.findall(r"^(\w+):\s*(.*)$", m.group(1), re.M))
    aud = {a.strip() for a in fields.get("audience", "").split(",") if a.strip()}
    if not aud:
        fail(f"{path}: front matter names no audience")
    for a in aud - AUDIENCES:
        fail(f"{path}: unknown audience {a!r}; one of {', '.join(sorted(AUDIENCES))}")
    if fields.get("type") not in TYPES:
        fail(f"{path}: front matter type {fields.get('type')!r} is not one of {', '.join(sorted(TYPES))}")


def check_index(pages: list[str]) -> None:
    index = open("docs/index.md", encoding="utf-8").read()
    decisions = open("docs/decisions/index.md", encoding="utf-8").read()
    listed = {os.path.normpath(os.path.join("docs", u.partition("#")[0])) for _, u in LINK_RE.findall(index)}
    listed |= {os.path.normpath(os.path.join("docs/decisions", u.partition("#")[0])) for _, u in LINK_RE.findall(decisions)}
    for p in pages:
        if p in ("docs/index.md", "docs/decisions/index.md"):
            continue
        if p.startswith("docs/releases/") or p.startswith("docs/reference/store-api/") or p.startswith("docs/reference/crds/"):
            continue  # listed by directory or by their own index page
        if p not in listed:
            fail(f"{p}: not listed in docs/index.md")


def main() -> int:
    files = tracked_md()
    for f in files:
        check_file(f)
    pages = [f for f in files if f.startswith("docs/")]
    for p in pages:
        check_front_matter(p)
    check_index(pages)
    if failures:
        print("\n".join(failures))
        print(f"\nFAIL — lint-docs: {len(failures)} finding(s) in {len(files)} Markdown files", file=sys.stderr)
        return 1
    print(f"OK — lint-docs: {len(files)} Markdown files, {len(pages)} pages under docs/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
