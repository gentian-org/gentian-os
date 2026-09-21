#!/usr/bin/env python3
"""Convert AppProfile manifests to ComponentProfile manifests plus store listings.

An AppProfile mixes three things a ComponentProfile keeps apart: what the
component is and needs (stays in the cluster), how it is presented (moves to the
store listing, outside the cluster), and how it is reached (ingress, additional
ingresses and browser-proxy routes become one `expose` list whose every entry
states its authMode).

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
                "categories", "keywords", "tile", "portalTiles"]
PACKAGE = ["chart", "deploymentMethod", "compositionRef", "valueMapping", "extraValues", "apiIntegration"]
CARRIED = {"provides": "provides", "optionalIntegrations": "integrations",
           "sidecars": "extensions", "backup": "backup", "customization": "customization"}
HANDLED = set(PRESENTATION) | set(PACKAGE) | set(CARRIED) | {
    "catalogueVersion", "trustTier", "kernelRequirements", "security", "appSecrets",
    "derivedSecretKeys", "ingress", "additionalIngresses", "browserProxy",
    "postInstallJob", "provisioning"}

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


def convert(doc, review):
    src = doc.get("spec", {})
    name = doc["metadata"]["name"]
    spec = {"tenancy": ["tenant"]}

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

    spec["package"] = {k: src[k] for k in PACKAGE if k in src}
    if not any(k in spec["package"] for k in ("chart", "compositionRef", "apiIntegration")):
        review.note("package: no chart, composition or API integration — the profile cannot be admitted")

    requires = {}
    if src.get("kernelRequirements"):
        requires["contracts"] = src["kernelRequirements"]
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
    names = [e["name"] for e in expose]
    for dup in sorted({n for n in names if names.count(n) > 1}):
        review.note(f"expose: two entries are named {dup}; names must be unique")
    if expose:
        spec["expose"] = expose

    for old in ("sidecars", "backup", "customization"):
        if src.get(old):
            spec[CARRIED[old]] = src[old]
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
    reviews, seen = [], {}
    for path in sorted(args.source.rglob("*.yaml")):
        try:
            docs = list(yaml.safe_load_all(path.read_text()))
        except yaml.YAMLError as err:
            print(f"skip {path}: {err}", file=sys.stderr)
            continue
        for doc in docs:
            if not isinstance(doc, dict) or doc.get("kind") != "AppProfile":
                continue
            name = doc["metadata"]["name"]
            if name in seen:
                print(f"FAIL — {name} is defined in {seen[name]} and in {path}", file=sys.stderr)
                return 1
            seen[name] = path
            review = Review(name)
            profile, listing = convert(doc, review)
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
