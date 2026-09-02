#!/usr/bin/env python3
"""Render credentials.yaml into CredentialRequirement CRs.

The catalogue ships twice from one source. credentials.yaml travels with the
installer, because the installer needs to know what to prompt for before the
cluster exists; the generated CRs travel in the platform Configuration package,
because the on-cluster credential manager reads them from the API rather than
from a file it has no access to.

Two carriers is a drift hazard, so the second is generated and `make verify-gen`
fails when it is stale. Edit credentials.yaml and re-run `make gen-all`; never
edit the generated file.

Usage:
    python3 scripts/gen/gen-credential-requirements.py
    python3 scripts/gen/gen-credential-requirements.py --check   # CI parity check
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required: pip install pyyaml")

ROOT = Path(__file__).resolve().parents[2]   # scripts/gen/ -> repo root
SOURCE = ROOT / "credentials.yaml"
PLATFORMS = ROOT / "kernel" / "platforms.yaml"
TARGET = ROOT / "kernel" / "credentials" / "credential-requirements.yaml"

HEADER = """# GENERATED FILE — DO NOT EDIT.
#
# Rendered by scripts/gen/gen-credential-requirements.py from credentials.yaml.
# To change a requirement, edit credentials.yaml and run `make gen-all`.
#
# These are the catalogue's on-cluster carrier: the credential manager reads
# CredentialRequirement objects from the API, while the installer reads
# credentials.yaml from disk before any cluster exists. Same content, two
# carriers, one source.
"""

# Fields whose absence is meaningful rather than a default worth emitting.
OPTIONAL_SPEC_KEYS = ("description", "optional", "validate", "consumedBy")


# Namespace holding the satisfaction probes. Nothing else lives there.
PROBE_NAMESPACE = "gentian-system"


def build_probe(req):
    """An ExternalSecret whose only job is to answer "does this path exist".

    creationPolicy: None is the point. ESO still resolves the remote reference
    and still reports SecretSynced, but it creates no Secret — so satisfaction
    becomes observable without materialising cluster-wide credential material
    into a namespace that has no use for it.

    This is what makes "no controller" possible. Satisfaction is a Kubernetes
    condition that ESO maintains, so function-extra-resources can gate a
    Composition on it and the credential manager can read it, without anything
    bespoke holding an OpenBao token to poll with.
    """
    return {
        "apiVersion": "external-secrets.io/v1",
        "kind": "ExternalSecret",
        "metadata": {
            "name": f"credreq-{req['name']}",
            "namespace": PROBE_NAMESPACE,
            "labels": {
                "gentianos.io/credential-requirement": req["name"],
                "gentianos.io/credential-phase": req["phase"],
                "gentianos.io/credential-optional": str(req.get("optional", False)).lower(),
            },
        },
        "spec": {
            "refreshInterval": "1h",
            "secretStoreRef": {"name": "openbao", "kind": "ClusterSecretStore"},
            "target": {"creationPolicy": "None"},
            "data": [
                {
                    "secretKey": f["key"],
                    "remoteRef": {"key": req["vaultPath"], "property": f["key"]},
                }
                for f in req["fields"]
            ],
        },
    }


def dns_requirements(platforms):
    """The DNS providers' credentials, from the table that also defines their solvers.

    They are not written out in credentials.yaml because they are already
    written down: kernel/platforms.yaml gives each provider the Secret its
    cert-manager solver reads, the OpenBao path behind it and the fields it
    carries. Restating that here would be the same fact in two files, and the
    one that drifts is always the one nothing renders.

    Every provider is emitted, not only the one this cluster selected. The
    catalogue describes the platform; which entry applies to a given cluster is
    the installer's question, and it answers it from the same table.
    """
    reqs = []
    for name, profile in sorted((platforms.get("dnsProviders") or {}).items()):
        cred = profile.get("credential")
        if not cred:
            continue          # "none", and any provider whose access is not a secret
        req = {
            "name": f"acme-dns-{name}",
            "displayName": cred.get("displayName", f"{profile.get('displayName', name)} Credentials"),
            # Bootstrap only where the provider has a probe the installer can
            # actually run; see the note on cloudflare in kernel/platforms.yaml.
            "phase": cred.get("phase", "runtime"),
            "scope": "cluster",
            # Every one of them is optional: a cluster uses at most one, and a
            # cluster on HTTP-01 or a private CA uses none.
            "optional": True,
            "vaultPath": cred["vaultPath"],
            "fields": cred["fields"],
            "consumedBy": [{"kind": "XCluster", "name": "cluster"}],
        }
        if cred.get("description"):
            req["description"] = cred["description"]
        if cred.get("validate"):
            req["validate"] = {"type": cred["validate"]}
        reqs.append(req)
    return reqs


def build_documents(catalogue):
    """Map catalogue entries onto CredentialRequirement objects and their probes."""
    docs = []
    for req in catalogue.get("requirements", []):
        name = req["name"]

        spec = {
            "displayName": req["displayName"],
            "phase": req["phase"],
            "scope": req["scope"],
            **({"tenant": req["tenant"]} if req.get("tenant") else {}),
            "vaultPath": req["vaultPath"],
            "fields": req["fields"],
        }
        for key in OPTIONAL_SPEC_KEYS:
            if key in req:
                spec[key] = req[key]

        docs.append(
            {
                "apiVersion": "gentianos.io/v1alpha1",
                "kind": "CredentialRequirement",
                "metadata": {
                    "name": name,
                    "labels": {
                        # Lets the credential manager and any gating Composition
                        # select by phase and scope without parsing the spec.
                        "gentianos.io/credential-phase": req["phase"],
                        "gentianos.io/credential-scope": req["scope"],
                        **({"gentianos.io/tenant": req["tenant"]} if req.get("tenant") else {}),
                    },
                },
                "spec": spec,
            }
        )
        docs.append(build_probe(req))
    return docs


def render(catalogue):
    body = yaml.safe_dump_all(
        build_documents(catalogue),
        sort_keys=False,
        default_flow_style=False,
        width=100,
    )
    return HEADER + "\n" + body


def validate(catalogue):
    """Reject a catalogue the CRD schema or the plan's own rules would reject.

    Caught here rather than at admission because the installer reads
    credentials.yaml directly and never applies it, so nothing else would.
    """
    errors = []
    seen_names, seen_paths = set(), {}

    for req in catalogue.get("requirements", []):
        name = req.get("name", "<unnamed>")

        for key in ("name", "displayName", "phase", "scope", "vaultPath", "fields"):
            if not req.get(key):
                errors.append(f"{name}: missing required key '{key}'")

        if req.get("phase") not in ("bootstrap", "runtime"):
            errors.append(f"{name}: phase must be bootstrap or runtime")
        if req.get("scope") not in ("cluster", "tenant"):
            errors.append(f"{name}: scope must be cluster or tenant")

        # Scope is a class; tenant is an identity. A tenant-scoped requirement
        # with no tenant is visible to every tenant admin, which for a
        # tenant-proprietary repository credential is a disclosure. The CRD's CEL
        # rule rejects this at admission; catching it here names the catalogue
        # entry instead of the generated object.
        tenant = req.get("tenant", "")
        if req.get("scope") == "tenant" and not tenant:
            errors.append(f"{name}: scope is tenant, so tenant must name which one")
        if req.get("scope") == "cluster" and tenant:
            errors.append(f"{name}: tenant is set but scope is cluster")

        if name in seen_names:
            errors.append(f"{name}: duplicate requirement name")
        seen_names.add(name)

        # Sharing a path is legitimate — one token serving several consumers
        # means one rotation point — but the field sets must agree, or a write
        # through one requirement silently truncates the other.
        path = req.get("vaultPath")
        if path:
            keys = tuple(sorted(f.get("key", "") for f in req.get("fields", [])))
            if path in seen_paths and seen_paths[path][1] != keys:
                errors.append(
                    f"{name}: shares vaultPath '{path}' with "
                    f"'{seen_paths[path][0]}' but declares different fields"
                )
            seen_paths.setdefault(path, (name, keys))

        # The rule that keeps the installer's validator set small: a bootstrap
        # credential with no probe should be reclassified, not waved through.
        validate_type = (req.get("validate") or {}).get("type", "noop")
        if req.get("phase") == "bootstrap" and validate_type == "noop":
            if name != "master-password":
                errors.append(
                    f"{name}: phase 'bootstrap' with validate 'noop' — either give it a "
                    f"probe or reclassify it as runtime — bootstrap validators are "
                    f"held to curl and openssl, so anything needing more cannot be one"
                )

    return errors


def main():
    check = "--check" in sys.argv

    catalogue = yaml.safe_load(SOURCE.read_text())

    # The DNS providers' credentials are merged in from kernel/platforms.yaml
    # rather than restated in credentials.yaml — see dns_requirements. Merged
    # before validation, so they are held to the same rules as everything else.
    catalogue.setdefault("requirements", []).extend(
        dns_requirements(yaml.safe_load(PLATFORMS.read_text()))
    )

    errors = validate(catalogue)
    if errors:
        for e in errors:
            print(f"ERROR: {e}", file=sys.stderr)
        sys.exit(1)

    rendered = render(catalogue)

    if check:
        if not TARGET.exists():
            sys.exit(f"{TARGET} does not exist. Run `make gen-all`.")
        if TARGET.read_text() != rendered:
            sys.exit(
                f"{TARGET} is out of date with credentials.yaml "
                f"or kernel/platforms.yaml.\n"
                f"Run `make gen-all` and commit the result."
            )
        print(f"credential catalogue carriers agree ({len(catalogue['requirements'])} requirements)")
        return

    TARGET.parent.mkdir(parents=True, exist_ok=True)
    TARGET.write_text(rendered)
    print(f"wrote {TARGET.relative_to(ROOT)} ({len(catalogue['requirements'])} requirements)")


if __name__ == "__main__":
    main()
