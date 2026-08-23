#!/usr/bin/env bash
# step: A-04-prewarm
# phase: control-plane
# requires: A-03-namespaces
# provides: pre-pulled images on every node
# check: none — prewarming is a cache optimisation with no persistent artefact to test for; it is idempotent and cheap to repeat
# mutates: transient DaemonSet/Pods only — nothing that outlives the step

apply() {
    prewarm_cluster
}
