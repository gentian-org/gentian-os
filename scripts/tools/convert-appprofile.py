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

  convert-appprofile.py <profiles-dir> --out <dir> [--strict]

Writes <dir>/profiles/<name>.yaml, <dir>/listings/<name>.yaml and
<dir>/REVIEW.md.
"""
import argparse
import pathlib
import re
import sys
from urllib.parse import urlparse

import yaml

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


def tile_from_portal(pt, review, profile_name):
    label, translations = tile_label(pt.get("displayName"))
    if not label:
        label = pt.get("name") or profile_name
        review.note(f"tile/{pt.get('name')}: the portal tile had no displayName; used {label!r}")
    tile = {"displayName": label}
    if translations:
        tile["displayNames"] = translations
    # icon is required and is a glyph NAME, where the old profile carried an
    # SVG under spec.tile. The name is a guess from the tile's own id and a
    # person has to confirm the portal knows it.
    tile["icon"] = slug(pt.get("name") or profile_name)
    review.note(f"tile/{pt.get('name')}: icon set to {tile['icon']!r} from the tile id -- the old profile "
                "carried an SVG, not a glyph name; confirm the portal has this icon")
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
def expose_for_addon(addon, tiles, bases, review, profile_name):
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
                    "tile": tile_from_portal(pt, review, profile_name)})
    return out

def convert(doc, review, bases):
    src = doc.get("spec", {})
    name = doc["metadata"]["name"]
    # classes, not tenancy: the field was renamed when a profile stopped
    # saying where it may run and started saying what it may be certified as.
    spec = {"classes": ["app"]}

    if src.get("trustTier"):
        spec["trustTier"] = src["trustTier"]
    else:
        spec["trustTier"] = "experimental"
        review.note("trustTier was absent; set to experimental, the tier that claims nothing")
    if src.get("catalogueVersion"):
        spec["version"] = str(src["catalogueVersion"])
    else:
        spec["version"] = "0.0.0"
        review.note("catalogueVersion was absent; version set to 0.0.0")

    # package is exactly one of chart | composition | api | addon, plus the
    # value plumbing. deploymentMethod is dropped: the union says it now.
    package = {new: src[old] for old, new in PACKAGE_RENAME.items() if old in src}
    addon = (src.get("customization") or {}).get("addon")
    if addon:
        # Exactly one package. An addon runs nothing of its own, so a chart on
        # an addon is a leftover from when an addon could be installed
        # standalone -- eleven of the twenty carry one.
        dropped = [k for k in ("chart", "api") if k in package]
        for k in dropped:
            package.pop(k)
        package["addon"] = {"id": addon.get("id"), "of": addon.get("of")}
        if dropped:
            review.note(f"package: dropped {', '.join(sorted(dropped))} — an addon activates inside its "
                        "base and runs nothing of its own; these are from the standalone era")
    if src.get("compositionRef"):
        review.note(f"package: dropped compositionRef {src['compositionRef']!r} — the composition that "
                    "renders an app is chosen by the claim, and nothing reads this field")
    if src.get("deploymentMethod"):
        review.note(f"package: dropped deploymentMethod {src['deploymentMethod']!r} — delivery is read "
                    "from which package kind is present and can no longer contradict it")
    present = [k for k in ("chart", "composition", "api", "addon") if k in package]
    if len(present) != 1:
        review.note(f"package: {present or 'nothing'} — exactly one of chart, composition, api or addon "
                    "is required and the profile will be refused")
    spec["package"] = package

    requires = {}
    if src.get("kernelRequirements"):
        requires["services"] = src["kernelRequirements"]
    privileges = {}
    security = src.get("security") or {}
    for w in security.get("macWaivers", []):
        wname = slug(f"{w.get('policy')}-{w.get('scope')}")
        privileges.setdefault("podSecurity", []).append({
            "name": wname, "policy": w.get("policy"), "scope": w.get("scope"),
            "reason": "REVIEW: say why this component needs the waiver"})
        review.note(f"requires.privileges.podSecurity/{wname}: write the reason the security officer will read")
    for i, rule in enumerate(security.get("egress", [])):
        ename = f"egress-{i + 1}"
        privileges.setdefault("egress", []).append({
            "name": ename, "rule": rule,
            "reason": "REVIEW: say what this component reaches and why"})
        review.note(f"requires.privileges.egress/{ename}: write the reason the tenant administrator will "
                    "read, and give it a name that says where it goes")
    if privileges:
        requires["privileges"] = privileges
    if requires:
        spec["requires"] = requires

    for old, new in CARRIED.items():
        if old in ("provides", "optionalIntegrations") and src.get(old):
            spec[new] = src[old]

    secrets = {}
    if src.get("appSecrets"):
        secrets["generated"] = src["appSecrets"]
    if src.get("derivedSecretKeys"):
        secrets["derived"] = src["derivedSecretKeys"]
        review.note("secrets.derived: derived secrets rotate silently if the formula or its inputs change; "
                    "move to generated unless something external recomputes the value")
    if secrets:
        spec["secrets"] = secrets

    identity = (src.get("kernelRequirements") or {}).get("identity") or {}
    has_oidc = bool(identity.get("oidc") or identity.get("saml"))
    expose = []
    if src.get("ingress"):
        expose.append(expose_from_ingress(src["ingress"], "web", review, has_oidc))
    for ing in src.get("additionalIngresses", []):
        expose.append(expose_from_ingress(ing, slug(ing.get("subDomain") or ing.get("serviceName")), review, has_oidc))
    for route in src.get("browserProxy", []):
        expose.append(expose_from_proxy(route, review))
    # The tiles. A tile opens one host and one path, and the exposure decides
    # both, so a tile hangs off an exposure rather than off the profile.
    tiles = src.get("portalTiles") or []
    if addon:
        expose.extend(expose_for_addon(addon, tiles, bases, review, name))
    elif tiles:
        # The first tile belongs on the entry that serves the app's own host.
        # Any further tile is a second entry on the same backend: it is a
        # different path into the same app, and the old profile distinguished
        # them by linkSuffix alone.
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
            expose[0]["tile"] = tile_from_portal(tiles[0], review, name)
            for pt in tiles[1:]:
                extra = {k: v for k, v in expose[0].items() if k != "tile"}
                extra["name"] = slug(pt.get("name") or "tile")
                extra["tile"] = tile_from_portal(pt, review, name)
                expose.append(extra)

    names = [e["name"] for e in expose]
    for dup in sorted({n for n in names if names.count(n) > 1}):
        review.note(f"expose: two entries are named {dup}; names must be unique")
    if expose:
        spec["expose"] = expose

    # launch says how a person reaches this component at all.
    spec["launch"] = "tile" if any(e.get("tile") for e in expose) else "none"
    if spec["launch"] == "none" and not addon and src.get("ingress"):
        review.note("launch: none — the profile is reachable but advertises no tile; confirm that is "
                    "intended rather than a tile that failed to convert")

    for old in ("sidecars", "backup", "customization"):
        if not src.get(old):
            continue
        value = src[old]
        if old == "customization" and addon:
            # On this kind an addon is package.addon, and customization.addon
            # is refused. What is left of the block still belongs here.
            value = {k: v for k, v in value.items() if k != "addon"}
            if not value:
                continue
        spec[CARRIED[old]] = value
    hooks = {}
    if src.get("postInstallJob"):
        hooks["postInstall"] = src["postInstallJob"]
    if src.get("provisioning"):
        hooks["provisioning"] = src["provisioning"]
    if hooks:
        spec["hooks"] = hooks

    for key in sorted(set(src) - HANDLED):
        review.note(f"{key}: not a field this tool knows; it was dropped")

    profile = {"apiVersion": "gentianos.io/v1alpha1", "kind": "ComponentProfile",
               "metadata": {"name": name}, "spec": spec}
    for meta in ("labels", "annotations"):
        if doc["metadata"].get(meta):
            profile["metadata"][meta] = doc["metadata"][meta]
    listing = {"profile": name}
    listing.update({k: src[k] for k in PRESENTATION if k in src})
    return profile, listing


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source", type=pathlib.Path, help="directory searched recursively for AppProfile manifests")
    ap.add_argument("--out", type=pathlib.Path, required=True)
    ap.add_argument("--strict", action="store_true", help="exit 1 while any review item remains")
    args = ap.parse_args()

    (args.out / "profiles").mkdir(parents=True, exist_ok=True)
    (args.out / "listings").mkdir(parents=True, exist_ok=True)

    # Two passes. An addon's exposure names its base's Service, so every
    # profile's own ingress has to be known before any addon is converted --
    # and an addon is as likely to be read first as last.
    sources = []
    for path in sorted(args.source.rglob("*.yaml")):
        try:
            docs = list(yaml.safe_load_all(path.read_text()))
        except yaml.YAMLError as err:
            print(f"skip {path}: {err}", file=sys.stderr)
            continue
        for doc in docs:
            if isinstance(doc, dict) and doc.get("kind") == "AppProfile":
                sources.append((path, doc))

    bases = {}
    for _, doc in sources:
        ing = (doc.get("spec") or {}).get("ingress") or {}
        if ing.get("serviceName"):
            bases[doc["metadata"]["name"]] = {
                "service": ing["serviceName"],
                "port": ing.get("servicePort") or 80,
                "subDomain": ing.get("subDomain") if ing.get("subDomain") != "auto" else None,
            }

    reviews, seen = [], {}
    for path, doc in sources:
        name = doc["metadata"]["name"]
        if name in seen:
            print(f"FAIL — {name} is defined in {seen[name]} and in {path}", file=sys.stderr)
            return 1
        seen[name] = path
        review = Review(name)
        profile, listing = convert(doc, review, bases)
        (args.out / "profiles" / f"{name}.yaml").write_text(yaml.safe_dump(profile, sort_keys=False))
        (args.out / "listings" / f"{name}.yaml").write_text(yaml.safe_dump(listing, sort_keys=False, allow_unicode=True))
        reviews.append(review)

    open_items = sum(len(r.items) for r in reviews)
    lines = ["# Conversion review", "",
             f"{len(reviews)} profiles converted, {open_items} items a person has to decide.", ""]
    for r in reviews:
        if r.items:
            lines += [f"## {r.profile}", ""] + [f"- {i}" for i in r.items] + [""]
    (args.out / "REVIEW.md").write_text("\n".join(lines))
    print(f"{len(reviews)} profiles → {args.out}/profiles, listings → {args.out}/listings, "
          f"{open_items} review items → {args.out}/REVIEW.md")
    return 1 if (args.strict and open_items) else 0


if __name__ == "__main__":
    sys.exit(main())
