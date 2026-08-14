#!/usr/bin/env bash
# step: C-03-provider-helm
# phase: platform
# requires: C-02-root-appset
# provides: provider-helm reporting Healthy
# mutates: nothing — waits on a condition

# No check(): this step only waits. Running it against an already-Healthy
# provider returns immediately, so there is nothing to skip.

apply() {
    install_provider_helm
}
