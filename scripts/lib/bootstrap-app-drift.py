#!/usr/bin/env python3
# =============================================================================
# scripts/lib/bootstrap-app-drift.py — has a bootstrap Application drifted from
# its template?
# =============================================================================
# B-03's check() used to ask whether an Application object EXISTS. Once
# bootstrapped it therefore reported satisfied forever, and a change to any of
# the six templates it owns reached a fresh install and silently never reached
# an already-bootstrapped one. kernel-admin-dev sat Degraded for days on two
# fixes that were already in the repo and could not get to the cluster.
#
# The question this answers instead is "does the live object still contain what
# the template declares", which is a SUBSET test, not equality. Equality is the
# wrong test and measurably so: on a freshly installed cluster `kubectl diff`
# reports the globals Application as changed, on
#
#   generation: 691 -> 692      the dry-run apply's own increment
#   directory: {recurse: false} CRD schema defaults the API server adds
#   syncOptions: []
#
# None of that is drift. A check built on equality would re-apply that
# Application on every single run, forever, which is a worse failure than the
# one being fixed — it would make --force meaningless and mask real drift in
# noise.
#
# Fields the API server defaults, ArgoCD writes, or another controller adds are
# all outside what the template asserts, so they are ignored by construction
# rather than by a list of exceptions that would need maintaining.
#
# Usage: bootstrap-app-drift.py <rendered.yaml> <live.json>
#   exit 0  the live object contains everything the template declares
#   exit 1  it does not — the template has changed, or something overwrote it
#   exit 2  could not tell (unreadable input); the caller treats this as
#           "cannot determine" rather than as drift
# =============================================================================

import json
import sys


def drifted(want, got, path=""):
    """Every leaf in `want` must be present and equal in `got`.

    Returns a list of human-readable differences, empty when the live object
    satisfies the template.
    """
    out = []
    if isinstance(want, dict):
        if not isinstance(got, dict):
            return [f"{path or '.'}: expected a mapping, live has {type(got).__name__}"]
        for k, v in want.items():
            if k not in got:
                # Absent live, but only drift if the template asserts something
                # NON-ZERO. The API server does not store zero values, so a
                # template declaring `syncOptions: []` or
                # `directory: {recurse: false}` is asserting nothing the server
                # would keep -- and both of those are in the globals template,
                # which is why an equality check calls that Application drifted
                # on a cluster where it was applied seconds earlier.
                #
                # Recursing with an empty mapping rather than special-casing
                # each shape: a nested structure of zero values is itself zero,
                # and one whose leaves are not gets reported per leaf.
                out.extend(drifted(v, {} if isinstance(v, dict) else
                                   ([] if isinstance(v, list) else None),
                                   f"{path}.{k}"))
            else:
                out.extend(drifted(v, got[k], f"{path}.{k}"))
        return out

    if isinstance(want, list):
        if not isinstance(got, list):
            return [f"{path or '.'}: expected a list, live has {type(got).__name__}"]
        # Order matters for these: sources, and the ignoreDifferences entries
        # under them, are positional in Argo CD's own semantics — a reordered
        # sources list is a different Application, not the same one.
        if not want and not got:
            return out
        if len(want) != len(got):
            return [f"{path}: {len(want)} entries in the template, {len(got)} live"]
        for i, (w, g) in enumerate(zip(want, got)):
            out.extend(drifted(w, g, f"{path}[{i}]"))
        return out

    # A zero value the server did not store is not drift: absence and
    # default-false are the same thing to it.
    if got is None and not want:
        return out
    if want != got:
        # Multi-line strings are the interesting case and the one a naive
        # repr() renders useless: the inline Helm values of these Applications
        # run to dozens of lines, so two truncated reprs of them are identical
        # on screen and say nothing about what changed. Report the first line
        # that actually differs instead.
        if isinstance(want, str) and isinstance(got, str) and "\n" in (want + got):
            wl, gl = want.splitlines(), got.splitlines()
            for i in range(max(len(wl), len(gl))):
                a = wl[i] if i < len(wl) else "<absent>"
                b = gl[i] if i < len(gl) else "<absent>"
                if a != b:
                    out.append(f"{path}: line {i + 1} differs")
                    out.append(f"    template: {a.strip()[:70]}")
                    out.append(f"    live:     {b.strip()[:70]}")
                    return out
            out.append(f"{path}: differs in trailing whitespace")
            return out
        w, g = repr(want), repr(got)
        if len(w) > 60:
            w = w[:57] + "..."
        if len(g) > 60:
            g = g[:57] + "..."
        out.append(f"{path}: template has {w}, live has {g}")
    return out


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: bootstrap-app-drift.py <rendered.yaml> <live.json>", file=sys.stderr)
        return 2
    try:
        import yaml
    except ImportError:
        # PyYAML is not a declared installer dependency. Without it this cannot
        # tell, which is not the same as no drift -- the caller decides, and
        # decides conservatively.
        return 2

    try:
        with open(sys.argv[1], encoding="utf-8") as fh:
            want = [d for d in yaml.safe_load_all(fh) if d]
        with open(sys.argv[2], encoding="utf-8") as fh:
            live = json.load(fh)
    except Exception as exc:  # noqa: BLE001 - any read failure is "cannot tell"
        print(f"cannot compare: {exc}", file=sys.stderr)
        return 2

    # A template may render more than the Application: external-dns also
    # renders its Namespace and the ExternalSecret carrying its credential.
    # The Application is what B-03 tracks and what _bootstrap_app_object_name
    # names, so it is the one compared here. The others are Argo CD's to
    # reconcile once the Application exists.
    apps = [d for d in want if d.get("kind") == "Application"]
    if len(apps) != 1:
        print(f"expected one rendered Application, got {len(apps)}", file=sys.stderr)
        return 2
    app = apps[0]

    # metadata is compared only where the template asserts something: labels
    # and annotations it sets, not name/namespace/uid/resourceVersion, which
    # the object carries by definition.
    diffs = drifted(app.get("spec", {}), live.get("spec", {}), "spec")
    for key in ("labels", "annotations"):
        w = (app.get("metadata") or {}).get(key)
        if w:
            diffs.extend(drifted(w, (live.get("metadata") or {}).get(key, {}),
                                 f"metadata.{key}"))

    if diffs:
        for d in diffs[:12]:
            print(f"  {d}")
        if len(diffs) > 12:
            print(f"  ... and {len(diffs) - 12} more")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
