#!/usr/bin/env bash
# step: A-04-prewarm
# phase: control-plane
# requires: A-03-namespaces
# provides: pre-pulled images on every node
# mutates: transient DaemonSet/Pods only — nothing that outlives the step

# No check(): prewarming is a cache-warming optimisation with no persistent
# artefact to test for. It is idempotent and cheap enough to repeat.

apply() {
    prewarm_cluster
}
