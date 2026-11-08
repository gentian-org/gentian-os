#!/usr/bin/env python3
# =============================================================================
# scripts/tests/operator_network_policy.py — the assertions behind
# test-operator-network-policy.sh.
#
# Reads the rendered operator chart on stdin. Arguments say what the render
# was asked for: any of `director` and `usher` (the callers deployed), `off`
# (the policy was switched off), or `defaults` (take the callers from what
# the render itself deploys).
# =============================================================================
import sys

import yaml

COMPONENT = "app.kubernetes.io/component"


def main() -> int:
    args = set(sys.argv[1:])
    docs = [d for d in yaml.safe_load_all(sys.stdin) if isinstance(d, dict)]
    policies = [d for d in docs if d.get("kind") == "NetworkPolicy"]
    deployments = [d for d in docs if d.get("kind") == "Deployment"]

    if "off" in args:
        return fail("a NetworkPolicy was rendered with the switch off") if policies else 0
    if len(policies) != 1:
        return fail(f"{len(policies)} NetworkPolicies rendered, want exactly one")
    policy = policies[0]
    selector = policy["spec"]["podSelector"].get("matchLabels") or {}

    def selected(d):
        labels = d["spec"]["template"]["metadata"].get("labels") or {}
        return d["metadata"].get("namespace", "kernel-control") == policy["metadata"]["namespace"] and all(
            labels.get(k) == v for k, v in selector.items()
        )

    # It selects the operator and nothing else the chart deploys.
    hit = [d for d in deployments if selected(d)]
    if not selector or len(hit) != 1:
        return fail(f"the policy selects {[d['metadata']['name'] for d in hit]}, want the operator alone")
    operator = hit[0]
    containers = operator["spec"]["template"]["spec"]["containers"]
    if [c.get("command") for c in containers] != [["/manager"]]:
        return fail("the pod the policy selects is not the operator's")
    if policy["spec"].get("policyTypes") != ["Ingress"]:
        return fail("the policy must restrict ingress and say nothing about egress")

    served = {p["name"]: p["containerPort"] for c in containers for p in c.get("ports", [])}
    lifecycle = served.get("lifecycle")
    if lifecycle is None:
        return fail("the operator declares no lifecycle port")

    if "defaults" in args:
        deployed = {d["metadata"]["labels"].get(COMPONENT) for d in deployments}
        args = {name for name in ("director", "usher") if name in deployed}

    open_ports, guarded = set(), []
    for rule in policy["spec"].get("ingress") or []:
        ports = {p["port"] for p in rule.get("ports") or []}
        if not ports:
            return fail("a rule with no ports admits every port")
        if any(p.get("protocol") != "TCP" for p in rule["ports"]):
            return fail("every port must be named with its protocol")
        if rule.get("from"):
            guarded.append((ports, rule["from"]))
        elif "from" in rule:
            return fail("a rule with an empty `from` admits everybody")
        else:
            open_ports |= ports

    # Every other port the pod serves stays reachable; the listener's is not
    # among the open ones.
    others = {port for name, port in served.items() if name != "lifecycle"}
    if open_ports != others:
        return fail(f"ports open to anybody are {sorted(open_ports)}; the operator serves {sorted(others)} beside its listener")
    if lifecycle in open_ports:
        return fail("the listener's port is open to anybody")

    wanted = {name for name in ("director", "usher") if name in args}
    if not wanted:
        return fail("the listener's port is listed with nobody to admit") if guarded else 0
    if len(guarded) != 1 or guarded[0][0] != {lifecycle}:
        return fail(f"want one rule for the listener's port {lifecycle} alone, got {[sorted(g[0]) for g in guarded]}")
    peers = guarded[0][1]
    admitted = set()
    for peer in peers:
        # A pod selector on its own: pods of this namespace. A namespace
        # selector or an address block would admit more than that.
        if set(peer) != {"podSelector"}:
            return fail(f"a peer of the listener is not a pod selector alone: {sorted(peer)}")
        labels = peer["podSelector"].get("matchLabels") or {}
        if not all(labels.get(k) == v for k, v in selector.items() if k != COMPONENT):
            return fail("a peer of the listener is not a workload of this release")
        admitted.add(labels.get(COMPONENT))
    if admitted != wanted:
        return fail(f"the listener admits {sorted(map(str, admitted))}, want {sorted(wanted)}")
    # And each peer selects the pods of the Deployment it is named for.
    for name in wanted:
        match = [
            d for d in deployments
            if d["spec"]["template"]["metadata"]["labels"].get(COMPONENT) == name
            and d["metadata"].get("namespace", policy["metadata"]["namespace"]) == policy["metadata"]["namespace"]
        ]
        if len(match) != 1:
            return fail(f"no {name} Deployment in the operator's namespace for the policy to admit")
    return 0


def fail(message: str) -> int:
    print(f"        {message}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
