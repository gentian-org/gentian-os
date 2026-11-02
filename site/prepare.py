#!/usr/bin/env python3
"""Assemble the content the site builds from.

The pages live in the repositories; this script copies them into
site/content/ and rewrites their links so they resolve on the site:

  content/os/    gentian-os: docs/ as it is, plus the root pages
                 (GETTING-STARTED.md, CONTRIBUTING.md, LICENSING.md,
                 SECURITY.md, AGENTS.md) at the places listed in ROOT_PAGES
  content/apps/  gentian-apps: README.md and docs/
  content/ui/    gentian-ui: README.md, SECURITY.md and the docs of each app

A relative link to a page that is copied becomes a link to its copy. A
relative link to anything else (code, a chart, a LICENSE) becomes a link to
the file on GitHub. A GitHub link to a page that is copied becomes a site
link. docs/llms.txt is served from the site root.

Usage: prepare.py [--os PATH] [--apps PATH] [--ui PATH] [--branch NAME]
The default for --os is the repository this file sits in; --apps and --ui
default to GENTIAN_APPS_DIR and GENTIAN_UI_DIR, and are skipped when unset
or not present.
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
CONTENT = os.path.join(HERE, "content")
STATIC = os.path.join(HERE, "static")
GITHUB = "https://github.com/gentian-org"

# gentian-os root pages: repository path -> content path, extra front matter
ROOT_PAGES = {
    "GETTING-STARTED.md": ("getting-started.md", "audience: operator\ntype: tutorial\nsidebar_position: 2\nsidebar_label: Getting started"),
    "CONTRIBUTING.md": ("contributing/contributing.md", "audience: contributor\ntype: contributing"),
    "LICENSING.md": ("contributing/licensing.md", "audience: contributor, customer\ntype: contributing"),
    "SECURITY.md": ("security/reporting.md", "audience: auditor, operator\ntype: contributing"),
    "AGENTS.md": ("contributing/agents.md", "audience: contributor\ntype: contributing"),
    "README.md": ("index.md", None),  # not copied: the docs index is the home page; links to it go there
}
# sidebar order of the gentian-os sections
SECTIONS = [
    ("overview", "Overview"), ("guides", "Guides"), ("reference", "Reference"), ("concepts", "Concepts"),
    ("decisions", "Decisions"), ("security", "Security"), ("releases", "Releases"), ("contributing", "Contributing"),
]

LINK = re.compile(r"(\]\()([^)\s]+)(\))")
FM = re.compile(r"\A---\n(.*?)\n---\n", re.S)


class Source:
    """One repository: where its files are, where its copies go, how to link to it."""

    def __init__(self, name: str, root: str, out: str, branch: str):
        self.name, self.root, self.out, self.branch = name, root, out, branch
        self.copies: dict[str, str] = {}  # repo-relative source -> content-relative target
        self.link_only: set[str] = set()  # link targets that are not copied
        self.extra_fm: dict[str, str] = {}

    def add(self, src: str, dst: str, fm: str | None = None) -> None:
        if os.path.isfile(os.path.join(self.root, src)):
            self.copies[src] = dst
            if fm:
                self.extra_fm[src] = fm

    def github(self, path: str, directory: bool = False) -> str:
        kind = "tree" if directory else "blob"
        return f"{GITHUB}/{self.name}/{kind}/{self.branch}/{path}".rstrip("/")


def site_url(out_base: str, content_path: str) -> str:
    """The site route of a copied page: content/os/concepts/kernel.md -> /concepts/kernel."""
    p = content_path[:-3] if content_path.endswith(".md") else content_path
    if os.path.basename(p) == "index":
        p = os.path.dirname(p)
    return "/" + "/".join(x for x in (out_base, p) if x)


def rewrite(source: Source, src: str, text: str, all_sources: list[Source]) -> str:
    src_dir = os.path.dirname(src)
    dst_dir = os.path.dirname(source.copies[src])

    def sub(m: re.Match) -> str:
        pre, url, post = m.groups()
        if url.startswith("#") or url.startswith("mailto:"):
            return m.group(0)
        if url.startswith(("http://", "https://")):
            for s in all_sources:  # a GitHub link to a copied page becomes a site link
                g = re.match(re.escape(f"{GITHUB}/{s.name}/blob/") + r"[\w.-]+/([^#]+)(#.*)?$", url)
                if g and g.group(1) in s.copies:
                    return f"{pre}{site_url(s.out_base, s.copies[g.group(1)])}{g.group(2) or ''}{post}"
            return m.group(0)
        path, _, anchor = url.partition("#")
        anchor = f"#{anchor}" if anchor else ""
        directory = path.endswith("/")
        target = os.path.normpath(os.path.join(src_dir, path)) if path else src
        if target == ".":
            target = ""
        if target in source.copies:
            rel = os.path.relpath(source.copies[target], dst_dir or ".")
            return f"{pre}{rel}{anchor}{post}"
        if directory:
            dir_copy = source.dir_copies.get(target)
            if dir_copy is not None:
                rel = os.path.relpath(dir_copy or ".", dst_dir or ".")
                return f"{pre}{rel}/{anchor}{post}"
        return f"{pre}{source.github(target, directory)}{anchor}{post}"

    return LINK.sub(sub, text)


HTML_TAGS = {"a", "b", "br", "code", "details", "div", "em", "hr", "i", "img", "kbd", "li", "ol", "p", "pre", "span",
             "strong", "sub", "summary", "sup", "table", "tbody", "td", "th", "thead", "tr", "ul"}
PLACEHOLDER = re.compile(r"(?<!\\)<([A-Za-z][\w.-]*)>")


def escape_placeholders(text: str) -> str:
    """`<tenant>` in prose is a placeholder, not an HTML tag; CommonMark would drop it. Code is left alone."""
    out = []
    in_fence = False
    for line in text.split("\n"):
        if line.lstrip().startswith(("```", "~~~")):
            in_fence = not in_fence
            out.append(line)
            continue
        if in_fence:
            out.append(line)
            continue
        parts = re.split(r"(`[^`]*`)", line)  # code spans stay as they are
        for i in range(0, len(parts), 2):
            parts[i] = PLACEHOLDER.sub(lambda m: m.group(0) if m.group(1).lower() in HTML_TAGS else "\\" + m.group(0), parts[i])
        out.append("".join(parts))
    return "\n".join(out)


def with_front_matter(text: str, extra: str | None) -> str:
    if not extra:
        return text
    m = FM.match(text)
    if m:
        return f"---\n{m.group(1)}\n{extra}\n---\n" + text[m.end():]
    return f"---\n{extra}\n---\n\n" + text


def collect_os(s: Source) -> None:
    for dirpath, _, files in os.walk(os.path.join(s.root, "docs")):
        for f in files:
            full = os.path.relpath(os.path.join(dirpath, f), s.root)
            if f == "llms.txt":
                continue
            s.add(full, os.path.relpath(full, "docs"))
    for src, (dst, fm) in ROOT_PAGES.items():
        if fm is None:
            s.copies[src] = dst  # a link target only, nothing copied
            s.link_only.add(src)
        else:
            s.add(src, dst, fm)
    s.dir_copies = {"docs": ""}
    for d, _ in SECTIONS:
        s.dir_copies[f"docs/{d}"] = d
    s.dir_copies["docs/reference/store-api"] = "reference/store-api"


def collect_apps(s: Source) -> None:
    s.add("README.md", "index.md", "sidebar_position: 0")
    for f in sorted(os.listdir(os.path.join(s.root, "docs"))):
        if f.endswith(".md"):
            s.add(f"docs/{f}", f)
    s.dir_copies = {"docs": ""}


def collect_ui(s: Source) -> None:
    s.add("README.md", "index.md", "sidebar_position: 0")
    s.add("SECURITY.md", "security.md")
    apps = os.path.join(s.root, "apps")
    s.dir_copies = {}
    for app in sorted(os.listdir(apps)) if os.path.isdir(apps) else []:
        s.add(f"apps/{app}/README.md", f"{app}/index.md")
        docs = os.path.join(apps, app, "docs")
        if os.path.isdir(docs):
            for f in sorted(os.listdir(docs)):
                if f.endswith(".md"):
                    s.add(f"apps/{app}/docs/{f}", f"{app}/{f}")
            s.dir_copies[f"apps/{app}/docs"] = app
        s.dir_copies[f"apps/{app}"] = app


def write(s: Source, all_sources: list[Source]) -> int:
    n = 0
    for src, dst in s.copies.items():
        full = os.path.join(s.root, src)
        if src in s.link_only or not os.path.isfile(full):
            continue
        target = os.path.join(s.out, dst)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        if src.endswith(".md"):
            text = open(full, encoding="utf-8").read()
            text = with_front_matter(escape_placeholders(rewrite(s, src, text, all_sources)), s.extra_fm.get(src))
            open(target, "w", encoding="utf-8").write(text)
        else:
            shutil.copyfile(full, target)
        n += 1
    return n


def category(path: str, label: str, position: int, link_doc: str | None = None) -> None:
    """A section's label and order; a section without an index page gets a generated one, so a link to the directory resolves."""
    os.makedirs(path, exist_ok=True)
    if link_doc:
        link = f', "link": {{"type": "doc", "id": "{link_doc}"}}'
    elif os.path.isfile(os.path.join(path, "index.md")):
        link = ""
    else:
        link = f', "link": {{"type": "generated-index", "slug": "/{os.path.relpath(path, os.path.join(CONTENT, "os"))}"}}'
    open(os.path.join(path, "_category_.json"), "w").write(f'{{"label": "{label}", "position": {position}{link}}}\n')


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--os", default=os.path.dirname(HERE))
    ap.add_argument("--apps", default=os.environ.get("GENTIAN_APPS_DIR", ""))
    ap.add_argument("--ui", default=os.environ.get("GENTIAN_UI_DIR", ""))
    ap.add_argument("--branch", default=os.environ.get("GENTIAN_DOCS_BRANCH", "develop"))
    a = ap.parse_args()

    shutil.rmtree(CONTENT, ignore_errors=True)
    sources: list[Source] = []
    os_src = Source("gentian-os", os.path.abspath(a.os), os.path.join(CONTENT, "os"), a.branch)
    os_src.out_base = ""
    collect_os(os_src)
    sources.append(os_src)
    if a.apps and os.path.isdir(a.apps):
        apps = Source("gentian-apps", os.path.abspath(a.apps), os.path.join(CONTENT, "apps"), a.branch)
        apps.out_base = "apps"
        collect_apps(apps)
        sources.append(apps)
    if a.ui and os.path.isdir(a.ui):
        ui = Source("gentian-ui", os.path.abspath(a.ui), os.path.join(CONTENT, "ui"), a.branch)
        ui.out_base = "ui"
        collect_ui(ui)
        sources.append(ui)

    for s in sources:
        n = write(s, sources)
        print(f"{s.name}: {n} files -> {os.path.relpath(s.out, HERE)}")
    # every instance needs a directory, even an empty one, or the build fails
    for name in ("apps", "ui"):
        d = os.path.join(CONTENT, name)
        if not os.path.isdir(d):
            os.makedirs(d)
            open(os.path.join(d, "index.md"), "w").write(
                f"---\nsidebar_position: 0\n---\n\n# gentian-{name}\n\nThe pages of gentian-{name} are pulled in when the site is built in CI; "
                f"see [the repository]({GITHUB}/gentian-{name}).\n")

    for i, (d, label) in enumerate(SECTIONS, start=3):
        category(os.path.join(os_src.out, d), label, i)
    category(os.path.join(os_src.out, "reference", "store-api"), "Store API", 99, "reference/store-api/store-api")
    index = os.path.join(os_src.out, "index.md")
    home = with_front_matter(open(index, encoding="utf-8").read(), "sidebar_position: 1\nsidebar_label: Start here")
    open(index, "w", encoding="utf-8").write(home)

    os.makedirs(STATIC, exist_ok=True)
    llms = os.path.join(a.os, "docs", "llms.txt")
    if os.path.isfile(llms):
        shutil.copyfile(llms, os.path.join(STATIC, "llms.txt"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
