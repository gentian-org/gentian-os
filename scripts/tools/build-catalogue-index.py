#!/usr/bin/env python3
"""Write the index.yaml a catalogue source publishes beside its bundles.

    build-catalogue-index.py <catalogue-dir> [--catalogue <slug>]

A catalogue source is a directory served over https holding
``profiles/<name>.yaml`` (the bundle a director materialises: the
ComponentProfile and, after it in the same file, what travels with it) and
``listings/<name>.yaml`` (the presentation the App Store ingests). Neither
answers "what is in here", because an https server does not list a directory.
That is what the index is for, and without it a cluster can install from a
source by name but cannot browse it (AD-14).

What goes in is the technical half and nothing else: name, version, edition,
trust tier, digest. No display name, no description, no icon, no price. Those
belong to the store, which is where a person should be looking; a cluster
reproducing them would be a worse copy of a screen somebody else keeps
current, and this file is meant to be the fallback rather than a rival.

The digest is the sha256 of the bundle file as published -- the whole file,
the profile and its companions -- the same number the store ingests. For a source a cluster opened to a tenant it is the number the
install is checked against. For the store's own catalogue it is not -- there
the digest that governs arrives from the store over the store's TLS, and the
director drops this one before anybody sees it.

This tool indexes; it does not assemble or vet a bundle. What a bundle may
hold is decided on the cluster (docs/custom-catalogues.md §2,
internal/profilebundle/bundle.go), which refuses the install otherwise. Three
things that can be told from here without knowing those rules are told: a
file whose first document is not the profile it is named after, a companion
of a kind no bundle may hold, and a file too large to be carried.
"""

from __future__ import annotations

import argparse
import hashlib
import sys
from pathlib import Path

import yaml

# The editions a profile may declare. See gentian-os's api/v1alpha1 Edition.
EDITIONS = ("ce", "pe", "me", "ee")

# What a bundle may hold beside its profile, and how large it may be. The
# cluster's rules, of which these are the two a file's own bytes show:
# internal/profilebundle/bundle.go.
COMPANION_KINDS = ("Composition", "ConfigMap", "Customization", "OIDCPackCatalog")
MAX_BUNDLE = 180 << 10


def digest(path: Path) -> str:
    return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()


def edition_of(name: str, listing: dict) -> str:
    """The listing's edition, or the one the profile's own name ends in.

    The suffix is not a guess: every profile in the catalogue is named
    ``<family>-<edition>`` precisely so that the coordinate says which edition
    it is. A profile that says neither is ce, which is what an unmarked public
    profile has always been.
    """
    declared = str(listing.get("edition") or "").strip().lower()
    if declared:
        return declared
    tail = name.rsplit("-", 1)[-1].lower()
    return tail if tail in EDITIONS else "ce"


def build(directory: Path) -> tuple[list[dict], list[str]]:
    profiles = directory / "profiles"
    listings = directory / "listings"
    if not profiles.is_dir():
        raise SystemExit(f"FAIL — {profiles} is not a directory")

    entries: list[dict] = []
    complaints: list[str] = []
    for path in sorted(profiles.glob("*.yaml")):
        name = path.stem
        try:
            documents = [d for d in yaml.safe_load_all(path.read_text()) if d is not None]
        except yaml.YAMLError as exc:
            complaints.append(f"{name}: bundle does not parse: {exc}")
            continue
        # The profile is the first document; what follows travels with it.
        profile = documents[0] if documents and isinstance(documents[0], dict) else {}
        if profile.get("kind") != "ComponentProfile":
            complaints.append(f"{name}: the first document is not a ComponentProfile")
            continue
        if (profile.get("metadata") or {}).get("name") != name:
            complaints.append(f"{name}: the profile is named {(profile.get('metadata') or {}).get('name')!r}")
            continue
        strays = sorted({
            str(d.get("kind") if isinstance(d, dict) else type(d).__name__)
            for d in documents[1:]
            if not isinstance(d, dict) or d.get("kind") not in COMPANION_KINDS
        })
        if strays:
            complaints.append(
                f"{name}: holds {', '.join(strays)}; beside its profile a bundle holds only "
                f"{', '.join(COMPANION_KINDS)}"
            )
            continue
        if path.stat().st_size > MAX_BUNDLE:
            complaints.append(f"{name}: {path.stat().st_size} bytes, and a cluster carries at most {MAX_BUNDLE}")
            continue
        spec = profile.get("spec") or {}
        # Only what a tenant can install is listed. A system profile is part
        # of the kernel and installing one is not a thing anybody does from a
        # catalogue screen.
        if "tenant" not in (spec.get("tenancy") or ["tenant"]):
            continue

        listing_path = listings / f"{name}.yaml"
        listing = {}
        if listing_path.exists():
            try:
                listing = yaml.safe_load(listing_path.read_text()) or {}
            except yaml.YAMLError as exc:
                complaints.append(f"{name}: listing does not parse: {exc}")

        edition = edition_of(name, listing)
        if edition not in EDITIONS:
            complaints.append(f"{name}: edition {edition!r} is not one of {', '.join(EDITIONS)}")
            continue

        entry = {
            "name": name,
            "version": str(spec.get("version") or "0.0.0"),
            "edition": edition,
            "digest": digest(path),
        }
        if spec.get("trustTier"):
            entry["trustTier"] = str(spec["trustTier"])
        entries.append(entry)
    return entries, complaints


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("directory", type=Path, help="the catalogue source directory")
    ap.add_argument("--catalogue", default="", help="the slug, recorded as a comment only")
    ap.add_argument("--check", action="store_true",
                    help="fail if index.yaml is missing or out of date, and write nothing")
    args = ap.parse_args()

    entries, complaints = build(args.directory)
    for complaint in complaints:
        print(f"  refused  {complaint}", file=sys.stderr)

    header = "# Generated by build-catalogue-index.py. Do not edit.\n"
    if args.catalogue:
        header += f"# catalogue: {args.catalogue}\n"
    body = header + yaml.safe_dump({"entries": entries}, sort_keys=False, width=100)

    out = args.directory / "index.yaml"
    if args.check:
        if not out.exists() or out.read_text() != body:
            print(f"FAIL — {out} is missing or out of date; run build-catalogue-index.py",
                  file=sys.stderr)
            return 1
        print(f"index.yaml is current ({len(entries)} entries).")
        return 0

    out.write_text(body)
    by_edition: dict[str, int] = {}
    for e in entries:
        by_edition[e["edition"]] = by_edition.get(e["edition"], 0) + 1
    shown = ", ".join(f"{n} {ed}" for ed, n in sorted(by_edition.items()))
    print(f"wrote {out} ({len(entries)} entries: {shown or 'none'})")
    # A cluster lists ce and pe; the rest is the store's. Saying so here is
    # what stops somebody publishing a catalogue of me entries and wondering
    # why no cluster shows them.
    local = by_edition.get("ce", 0) + by_edition.get("pe", 0)
    print(f"  a cluster browsing this source will list {local}; "
          f"the other {len(entries) - local} are the App Store's to present.")
    return 1 if complaints else 0


if __name__ == "__main__":
    raise SystemExit(main())
