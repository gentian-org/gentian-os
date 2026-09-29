#!/usr/bin/env bash
# step: A-08-prewarm
# phase: control-plane
# requires: A-01-namespaces
# provides: pre-pulled images on every node, and the two cold-start races burned on a throwaway pod
# check: none — prewarming is a cache optimisation with no persistent artefact to test for; it is idempotent and cheap to repeat
# mutates: transient DaemonSet/Pods only — nothing that outlives the step

# Last in the control-plane phase, so it runs after the cluster can schedule
# and before B-01 brings up the first real workload.
#
# That ordering is the whole point. prewarm_cluster exists to hit two
# cold-start races on a pod nobody is waiting for, and on v5 the first PVC
# consumer is openbao-transit-0 in the seal namespace, delivered by B-01. With
# no prewarm those races are hit there instead, where they stall a step's wait
# rather than a throwaway pod -- which on microk8s is the hostpath first-bind
# wedge the function documents.
#
# Names no namespace of its own: prewarm_cluster works in kube-system, so this
# is v4's step unchanged.

apply() {
    prewarm_cluster
}
