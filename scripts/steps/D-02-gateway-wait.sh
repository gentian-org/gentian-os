#!/usr/bin/env bash
# step: D-02-gateway-wait
# phase: applications
# requires: D-01-operator
# provides: kernel Gateway reporting Programmed
# check: none — a pure wait, non-fatal by design; a Gateway that is not yet Programmed does not invalidate the steps that follow
# mutates: nothing — waits on a condition

apply() {
    wait_for_gateway_platform || warn "Gateway platform not ready; continuing."
}
