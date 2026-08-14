#!/usr/bin/env bash
# step: 27-gateway-wait
# requires: 26-operator
# provides: kernel Gateway reporting Programmed
# mutates: nothing — waits on a condition

# No check(): a pure wait. Non-fatal by design, matching the legacy
# `wait_for_gateway_platform || true` — a Gateway that is not yet Programmed
# does not invalidate the steps that follow.

apply() {
    wait_for_gateway_platform || warn "Gateway platform not ready; continuing."
}
