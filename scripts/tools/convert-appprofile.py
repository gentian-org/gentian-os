#!/usr/bin/env python3
"""Convert AppProfile manifests to ComponentProfile manifests plus store listings.

An AppProfile mixes three things a ComponentProfile keeps apart: what the
component is and needs (stays in the cluster), how it is presented (moves to the
store listing, outside the cluster), and how it is reached (ingress, additional
ingresses and browser-proxy routes become one `expose` list whose every entry
states its authMode).

A TILE is the fourth thing, and it does not follow presentation out of the
cluster. A tile carries `relation` -- the permission a person must hold to see
it -- and that is an authorization question the operator has to be able to
read. So portalTiles become `expose[].tile`, on the entry that serves the host
the tile opens. An ADDON has no host of its own: it names its base's Service
through `backend.component`, which is what lets twenty addon profiles keep the
tiles that are their only user-visible surface.

The conversion is mechanical wherever the AppProfile says enough. Where it does
not, the tool writes the safe value and says so: every line of the review report
is a decision a person has to look at before the profile ships, and the tool
exits non-zero with --strict while any remain.

COMMENTS SURVIVE. A quarter of the catalogue is design rationale — 949 inline
comments across 33 profiles, recording why each one is built the way it is —
and a safe_load/safe_dump round trip deletes every one of them. That has
happened here before: sync-profile-tile.py's own docstring records dropping 118
lines from docmost-ce for the sake of inlining one base64 string. So this reads
and writes with ruamel's round-trip mode, and moving a field to its new parent
moves the comment attached to it.

  convert-appprofile.py <profiles-dir> --out <dir> [--strict]
  convert-appprofile.py <profiles-dir> --in-place [--strict]

--out writes <dir>/profiles/<name>.yaml, <dir>/listings/<name>.yaml and
<dir>/REVIEW.md. --in-place rewrites each profile.yaml where it stands and puts
the listing beside it, which is what the real conversion does.
"""
import argparse
import io
import pathlib
import re
import sys
from urllib.parse import urlparse

import ruamel.yaml
from ruamel.yaml.comments import CommentedMap, CommentedSeq

# Round trip: preserves comments, key order, block scalars and quoting.
_yaml = ruamel.yaml.YAML(typ="rt")
_yaml.preserve_quotes = True
_yaml.width = 4096          # never re-wrap a line somebody chose the shape of
_yaml.indent(mapping=2, sequence=4, offset=2)


def _load_all(path):
    with io.open(path, encoding="utf-8") as fh:
        return list(_yaml.load_all(fh))


def _dump(doc, path):
    with io.open(path, "w", encoding="utf-8") as fh:
        _yaml.dump(doc, fh)


def _dump_str(doc):
    buf = io.StringIO()
    _yaml.dump(doc, buf)
    return buf.getvalue()


def move(src, skey, dst, dkey=None):
    """Move src[skey] to dst[dkey], carrying its comment.

    ruamel keeps a key's comment on the PARENT map, so popping a value leaves
    the comment behind and the rationale is lost exactly where it mattered.
    This moves both.
    """
    if src is None or skey not in src:
        return False
    dkey = dkey or skey
    dst[dkey] = src[skey]
    comment = getattr(src, "ca", None)
    if comment is not None and skey in comment.items:
        dst.ca.items[dkey] = comment.items.pop(skey)
    del src[skey]
    return True


def carry_comment(src, skey, dst, dkey):
    """Copy a key's comment without moving its value, for a field that is
    rebuilt rather than moved -- an ingress becoming an expose entry."""
    comment = getattr(src, "ca", None)
    if comment is not None and skey in comment.items and dkey in dst:
        dst.ca.items[dkey] = comment.items[skey]

PRESENTATION = ["displayName", "description", "family", "edition", "author", "license",
                "categories", "keywords", "tile"]
# portalTiles is deliberately NOT presentation: see the module docstring.
# compositionRef is NOT carried over. The composition that renders an app is
# chosen by the claim, never by the profile -- app-default reads the profile
# and no code anywhere reads compositionRef, which is why the migration table
# records it as "nothing, unused".
PACKAGE_RENAME = {"chart": "chart", "apiIntegration": "api",
                  "valueMapping": "valueMapping", "extraValues": "extraValues"}
CARRIED = {"provides": "provides", "optionalIntegrations": "integrations",
           "sidecars": "extensions", "backup": "backup", "customization": "customization"}
HANDLED = set(PRESENTATION) | set(PACKAGE_RENAME) | set(CARRIED) | {
    "catalogueVersion", "trustTier", "kernelRequirements", "security", "appSecrets",
    "derivedSecretKeys", "ingress", "additionalIngresses", "browserProxy",
    "postInstallJob", "provisioning", "portalTiles", "deploymentMethod"}

# What a browser-proxy authMode meant, in the words of the new enum.
PROXY_AUTH = {"": "oidc", "forward-bearer": "oidc", "session": "oidc", "none": "none", "bearer": "bearer"}


# The tile every component ships when its author has no art yet. Read from the
# app template rather than copied, so the placeholder a converted profile gets
# is the same one a new profile starts from.
def _placeholder_logo():
    import base64
    for root in (pathlib.Path(__file__).resolve().parents[2],
                 pathlib.Path.home() / "develop" / "gentian-apps"):
        svg = root / "apps" / "_template" / "profile" / "assets" / "tile.svg"
        if svg.is_file():
            return "data:image/svg+xml;base64," + base64.b64encode(svg.read_bytes()).decode()
    return ""


PLACEHOLDER_LOGO = _placeholder_logo()


def slug(text):
    return re.sub(r"[^a-z0-9]+", "-", str(text).lower()).strip("-")[:40] or "x"


class Review:
    def __init__(self, profile):
        self.profile, self.items = profile, []

    def note(self, text):
        self.items.append(text)


def expose_from_ingress(ing, name, review, has_oidc):
    entry = {"name": name, "surface": "gateway", "authMode": "oidc"}
    sub = ing.get("subDomain")
    if sub and sub != "auto":
        entry["subDomain"] = sub
    entry["backend"] = {"service": ing.get("serviceName") or "REVIEW", "port": ing.get("servicePort") or 80}
    if not ing.get("serviceName"):
        review.note(f"expose/{name}: the ingress named no service; backend.service is a placeholder")
    if not has_oidc:
        review.note(f"expose/{name}: the profile requests no OIDC or SAML client, so authMode oidc is the edge "
                    "session only — confirm the app needs no login of its own, or that it is an API "
                    "that should be bearer/jwt")
    if ing.get("annotations"):
        review.note(f"expose/{name}: dropped ingress annotations {sorted(ing['annotations'])} — body size "
                    "and timeouts are the gateway's policy now")
    return entry


def expose_from_proxy(route, review):
    name = slug(route.get("path", "proxy"))
    target = urlparse(route.get("target", ""))
    mode = PROXY_AUTH.get(route.get("authMode", ""))
    if mode is None:
        mode = "oidc"
        review.note(f"expose/{name}: unknown browserProxy authMode {route.get('authMode')!r}; set to oidc")
    entry = {"name": name, "surface": "gateway", "authMode": mode,
             "paths": ["/" + str(route.get("path", "")).strip("/")]}
    if route.get("stripPrefix"):
        entry["stripPrefix"] = True
    entry["backend"] = {"service": target.hostname or "REVIEW",
                        "port": target.port or (443 if target.scheme == "https" else 80)}
    if target.path not in ("", "/"):
        review.note(f"expose/{name}: the proxy target carried a path ({target.path}) that a backend "
                    "reference cannot; check the route still reaches the right prefix")
    if route.get("authMode", "") in ("", "forward-bearer"):
        review.note(f"expose/{name}: was forward-bearer — the backend received the user's token. It now "
                    "gets identity headers; set forwardToken only if it calls the director as the user "
                    "(platform tier only)")
    return entry



# A tile's label was a map of locale to string; it is now a required fallback
# plus an optional map. en_US is the fallback where there is one, because the
# catalogue is written in English and every other locale is a translation of
# it; where there is none, any entry is better than none.
def tile_label(display):
    if isinstance(display, str):
        return display, {}
    display = display or {}
    fallback = display.get("en_US") or next(iter(display.values()), "")
    translations = {k: v for k, v in display.items() if v and v != fallback}
    return fallback, translations


# linkSuffix was appended to the app's base URL, so it could start with ? as
# well as with /. tile.path is a path and is anchored ^/, so a bare query
# becomes /?... -- the same URL, spelled the way the field requires.
def tile_path(suffix):
    suffix = (suffix or "").strip()
    if not suffix:
        return ""
    return suffix if suffix.startswith("/") else "/" + suffix


def tile_from_portal(pt, review, profile_name, profile_tile=None):
    label, translations = tile_label(pt.get("displayName"))
    if not label:
        label = pt.get("name") or profile_name
        review.note(f"tile/{pt.get('name')}: the portal tile had no displayName; used {label!r}")
    tile = {"displayName": label}
    if translations:
        tile["displayNames"] = translations
    # The tile's image is the SVG the component ships. AppProfile kept it in
    # two places: spec.tile for the whole profile, and an optional override on
    # the portal tile itself. The override wins, exactly as it did.
    art = pt.get("tile") or profile_tile or {}
    if art.get("logo"):
        tile["logo"] = art["logo"]
        if art.get("image"):
            tile["image"] = art["image"]
    elif art.get("image"):
        tile["image"] = art["image"]
        review.note(f"tile/{pt.get('name')}: the profile names {art['image']} but never inlined it; run "
                    "gentian-apps' scripts/sync-profile-tile.py, because the cluster reads logo")
    else:
        # The profile named a built-in glyph, and there are no built-in glyphs
        # any more. The placeholder keeps the profile admissible rather than
        # leaving a component that cannot be installed at all; it is meant to
        # look unfinished, and the review line says to replace it.
        tile["logo"] = PLACEHOLDER_LOGO
        tile["image"] = "assets/tile.svg"
        was = f" The old profile named the built-in glyph {art['icon']!r}." if art.get("icon") else ""
        review.note(f"tile/{pt.get('name')}: no SVG, so the placeholder was used.{was} Draw this "
                    "component's own art into assets/tile.svg and re-run "
                    "gentian-apps' scripts/sync-profile-tile.py")
    path = tile_path(pt.get("linkSuffix"))
    if path:
        tile["path"] = path
    # relation is required: a tile with no question is a link shown to
    # everyone. allowedGroup said the same thing in Keycloak's vocabulary.
    tile["relation"] = "can_launch"
    group = pt.get("allowedGroup")
    if group and group not in ("Domain Users", "App Users"):
        review.note(f"tile/{pt.get('name')}: allowedGroup was {group!r}; relation is can_launch, which is "
                    "entitlement to the app. Narrow it if that group meant something else")
    if pt.get("linkTarget") and pt["linkTarget"] != "embedded":
        review.note(f"tile/{pt.get('name')}: linkTarget {pt['linkTarget']!r} has no equivalent; the portal "
                    "decides how a tile opens")
    return tile


# An addon publishes nothing of its own. Its tile opens the base, so its
# exposure names the base's Service -- which is what backend.component is for.
def expose_for_addon(addon, tiles, bases, review, profile_name, profile_tile=None):
    base = addon.get("of") or ""
    service = bases.get(base, {}).get("service")
    port = bases.get(base, {}).get("port", 80)
    if not service:
        service = "REVIEW"
        review.note(f"expose: the addon activates into {base!r}, whose Service this tool could not find; "
                    "backend.service is a placeholder")
    out = []
    for i, pt in enumerate(tiles):
        name = slug(pt.get("name") or f"tile-{i + 1}")
        out.append({"name": name, "surface": "gateway", "authMode": "oidc",
                    "subDomain": bases.get(base, {}).get("subDomain") or slug(base),
                    "backend": {"component": base, "service": service, "port": port},
                    "tile": tile_from_portal(pt, review, profile_name, profile_tile)})
    return out

def convert(doc, review, bases):
    """Rewrite one AppProfile document as a ComponentProfile plus a listing.

    Fields are MOVED rather than copied into a fresh map, so the comment a
    profile author wrote above a field travels to wherever that field now
    lives. What is left in the source at the end is, by construction, what
    this tool did not handle — which is how the review report is honest rather
    than optimistic.
    """
    src = doc.get("spec") or CommentedMap()
    name = doc["metadata"]["name"]
    spec = CommentedMap()

    # classes, not tenancy: the field was renamed when a profile stopped
    # saying where it may run and started saying what it may be certified as.
    spec["classes"] = CommentedSeq(["app"])

    if "trustTier" in src:
        move(src, "trustTier", spec)
    else:
        spec["trustTier"] = "experimental"
        review.note("trustTier was absent; set to experimental, the tier that claims nothing")
    if "catalogueVersion" in src:
        move(src, "catalogueVersion", spec, "version")
        spec["version"] = str(spec["version"])
    else:
        spec["version"] = "0.0.0"
        review.note("catalogueVersion was absent; version set to 0.0.0")

    # package is exactly one of chart | composition | api | addon, plus the
    # value plumbing. deploymentMethod is dropped: the union says it now.
    package = CommentedMap()
    for old, new_key in PACKAGE_RENAME.items():
        move(src, old, package, new_key)

    customization = src.get("customization")
    addon = (customization or {}).get("addon")
    if addon is not None:
        dropped = [k for k in ("chart", "api") if k in package]
        for k in dropped:
            del package[k]
        move(customization, "addon", package, "addon")
        if dropped:
            review.note(f"package: dropped {', '.join(sorted(dropped))} — an addon activates inside its "
                        "base and runs nothing of its own; these are from the standalone era")
        if customization is not None and not customization:
            del src["customization"]

    if "compositionRef" in src:
        review.note(f"package: dropped compositionRef {src['compositionRef']!r} — the composition that "
                    "renders an app is chosen by the claim, and nothing reads this field")
        del src["compositionRef"]
    if "deploymentMethod" in src:
        review.note(f"package: dropped deploymentMethod {src['deploymentMethod']!r} — delivery is read "
                    "from which package kind is present and can no longer contradict it")
        del src["deploymentMethod"]

    present = [k for k in ("chart", "composition", "api", "addon") if k in package]
    if len(present) != 1:
        review.note(f"package: {present or 'nothing'} — exactly one of chart, composition, api or addon "
                    "is required and the profile will be refused")
    spec["package"] = package

    # requires: the services the platform owes, and the privileges it must be
    # asked for.
    requires = CommentedMap()
    move(src, "kernelRequirements", requires, "services")

    privileges = CommentedMap()
    security = src.get("security") or CommentedMap()
    for w in security.get("macWaivers") or []:
        wname = slug(f"{w.get('policy')}-{w.get('scope')}")
        privileges.setdefault("podSecurity", CommentedSeq()).append(CommentedMap([
            ("name", wname), ("policy", w.get("policy")), ("scope", w.get("scope")),
            ("reason", "REVIEW: say why this component needs the waiver")]))
        review.note(f"requires.privileges.podSecurity/{wname}: write the reason the security officer will read")
    for i, rule in enumerate(security.get("egress") or []):
        ename = f"egress-{i + 1}"
        privileges.setdefault("egress", CommentedSeq()).append(CommentedMap([
            ("name", ename), ("rule", rule),
            ("reason", "REVIEW: say what this component reaches and why")]))
        review.note(f"requires.privileges.egress/{ename}: write the reason the tenant administrator will "
                    "read, and give it a name that says where it goes")
    if privileges:
        requires["privileges"] = privileges
        # The old block's comment explained why the app needs what it needs,
        # which is exactly what the approver now reads.
        carry_comment(src, "security", requires, "privileges")
    if "security" in src:
        del src["security"]
    if requires:
        spec["requires"] = requires

    move(src, "provides", spec, "provides")
    move(src, "optionalIntegrations", spec, "integrations")

    secrets = CommentedMap()
    move(src, "appSecrets", secrets, "generated")
    if move(src, "derivedSecretKeys", secrets, "derived"):
        review.note("secrets.derived: derived secrets rotate silently if the formula or its inputs change; "
                    "move to generated unless something external recomputes the value")
    if secrets:
        spec["secrets"] = secrets

    # expose: one ingress, any additional ingresses, and the browser-proxy
    # routes, each stating the authMode the old shape left implicit.
    identity = (requires.get("services") or {}).get("identity") or {}
    has_oidc = bool(identity.get("oidc") or identity.get("saml"))
    expose = CommentedSeq()
    if src.get("ingress"):
        expose.append(expose_from_ingress(src["ingress"], "web", review, has_oidc))
    for ing in src.get("additionalIngresses") or []:
        expose.append(expose_from_ingress(ing, slug(ing.get("subDomain") or ing.get("serviceName")),
                                          review, has_oidc))
    for route in src.get("browserProxy") or []:
        expose.append(expose_from_proxy(route, review))

    tiles = src.get("portalTiles") or []
    if addon is not None:
        expose.extend(expose_for_addon(addon, tiles, bases, review, name, src.get("tile")))
    elif tiles:
        if not expose:
            if "api" in package:
                review.note("portalTiles: an API-delivered entry has no Service, so there is no exposure to "
                            "hang its tile on. The tile opens package.api.baseUrl through the portal-proxy "
                            "runtime; decide how that is declared before this profile ships, or the entry "
                            "installs and is invisible")
            else:
                review.note("portalTiles: the profile has tiles but no ingress and is not an addon; "
                            "the tiles were dropped because there is no host to open")
        else:
            expose[0]["tile"] = tile_from_portal(tiles[0], review, name, src.get("tile"))
            for pt in tiles[1:]:
                extra = CommentedMap((k, v) for k, v in expose[0].items() if k != "tile")
                extra["name"] = slug(pt.get("name") or "tile")
                extra["tile"] = tile_from_portal(pt, review, name, src.get("tile"))
                expose.append(extra)

    names = [e["name"] for e in expose]
    for dup in sorted({n for n in names if names.count(n) > 1}):
        review.note(f"expose: two entries are named {dup}; names must be unique")
    if expose:
        spec["expose"] = expose
        # Whatever the author wrote above the ingress explains the surface,
        # and the first expose entry IS that surface.
        carry_comment(src, "ingress", spec, "expose")
    for gone in ("ingress", "additionalIngresses", "browserProxy", "portalTiles"):
        if gone in src:
            del src[gone]

    move(src, "sidecars", spec, "extensions")

    hooks = CommentedMap()
    move(src, "postInstallJob", hooks, "postInstall")
    move(src, "provisioning", hooks, "provisioning")
    if hooks:
        spec["hooks"] = hooks

    move(src, "backup", spec, "backup")
    move(src, "customization", spec, "customization")

    # launch says how a person reaches this component at all.
    spec["launch"] = "tile" if any(e.get("tile") for e in expose) else "none"
    if spec["launch"] == "none" and addon is None and "expose" in spec:
        review.note("launch: none — the profile is reachable but advertises no tile; confirm that is "
                    "intended rather than a tile that failed to convert")

    # Presentation leaves the cluster, comments and all.
    listing = CommentedMap()
    listing["profile"] = name
    for key in PRESENTATION:
        move(src, key, listing)

    # By construction, anything still here is unhandled.
    for key in list(src.keys()):
        review.note(f"{key}: not a field this tool knows; it was dropped")
        del src[key]

    profile = CommentedMap()
    profile["apiVersion"] = doc.get("apiVersion", "gentianos.io/v1alpha1")
    profile["kind"] = "ComponentProfile"
    profile["metadata"] = doc["metadata"]
    profile["spec"] = spec
    # The document's own header — the block above apiVersion, which is where a
    # profile records why it is built the way it is. The kind is renamed in it
    # too: a header that still says "AppProfile" above a ComponentProfile
    # reads as an oversight a year later, and every one of these headers names
    # the kind in its first line.
    if getattr(doc, "ca", None) is not None and doc.ca.comment:
        profile.ca.comment = _rename_kind_in_comment(doc.ca.comment)
    return profile, listing


def _rename_kind_in_comment(comment):
    """AppProfile -> ComponentProfile inside a preserved comment block."""
    for token in comment or []:
        for item in (token if isinstance(token, list) else [token]):
            if item is not None and getattr(item, "value", None):
                item.value = item.value.replace("AppProfile", "ComponentProfile")
    return comment


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source", type=pathlib.Path, help="directory searched recursively for AppProfile manifests")
    ap.add_argument("--out", type=pathlib.Path, help="write the results to a directory instead of in place")
    ap.add_argument("--in-place", action="store_true",
                    help="rewrite each profile.yaml where it stands, and put its listing beside it")
    ap.add_argument("--strict", action="store_true", help="exit 1 while any review item remains")
    args = ap.parse_args()
    if not args.out and not args.in_place:
        ap.error("one of --out or --in-place is required")

    # Two passes. An addon's exposure names its base's Service, so every
    # profile's own ingress has to be known before any addon is converted --
    # and an addon is as likely to be read first as last.
    sources = []
    for path in sorted(args.source.rglob("*.yaml")):
        try:
            docs = _load_all(path)
        except ruamel.yaml.YAMLError as err:
            print(f"skip {path}: {err}", file=sys.stderr)
            continue
        for index, doc in enumerate(docs):
            if isinstance(doc, dict) and doc.get("kind") == "AppProfile":
                sources.append((path, index, docs, doc))

    bases = {}
    for _, _, _, doc in sources:
        ing = (doc.get("spec") or {}).get("ingress") or {}
        if ing.get("serviceName"):
            bases[doc["metadata"]["name"]] = {
                "service": ing["serviceName"],
                "port": ing.get("servicePort") or 80,
                "subDomain": ing.get("subDomain") if ing.get("subDomain") != "auto" else None,
            }

    if args.out:
        (args.out / "profiles").mkdir(parents=True, exist_ok=True)
        (args.out / "listings").mkdir(parents=True, exist_ok=True)

    reviews, seen = [], {}
    for path, index, docs, doc in sources:
        name = doc["metadata"]["name"]
        if name in seen:
            print(f"FAIL — {name} is defined in {seen[name]} and in {path}", file=sys.stderr)
            return 1
        seen[name] = path
        review = Review(name)
        profile, listing = convert(doc, review, bases)
        reviews.append(review)

        if args.in_place:
            # The converted profile replaces the document it came from, in the
            # file it came from: a multi-document file keeps its other
            # documents, and git shows a diff of one profile rather than a
            # delete and an add.
            docs[index] = profile
            with io.open(path, "w", encoding="utf-8") as fh:
                _yaml.dump_all(docs, fh)
            _dump(listing, path.parent / "listing.yaml")
        else:
            _dump(profile, args.out / "profiles" / f"{name}.yaml")
            _dump(listing, args.out / "listings" / f"{name}.yaml")

    open_items = sum(len(r.items) for r in reviews)
    lines = ["# Conversion review", "",
             f"{len(reviews)} profiles converted, {open_items} items a person has to decide.", ""]
    for r in reviews:
        if r.items:
            lines += [f"## {r.profile}", ""] + [f"- {i}" for i in r.items] + [""]
    report = (args.out / "REVIEW.md") if args.out else (args.source / "CONVERSION-REVIEW.md")
    report.write_text("\n".join(lines), encoding="utf-8")
    where = "in place" if args.in_place else f"{args.out}/profiles"
    print(f"{len(reviews)} profiles → {where}, {open_items} review items → {report}")
    return 1 if (args.strict and open_items) else 0


if __name__ == "__main__":
    sys.exit(main())
