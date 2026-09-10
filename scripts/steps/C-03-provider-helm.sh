#!/usr/bin/env bash
# step: C-03-provider-helm
# phase: platform
# requires: C-02-root-appset
# provides: provider-helm reporting Healthy
# check: none — a pure wait; running it against an already-Healthy provider returns immediately, so there is nothing to skip
# mutates: nothing — waits on a condition

apply() {
    install_provider_helm
}
