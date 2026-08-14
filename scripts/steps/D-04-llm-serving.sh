#!/usr/bin/env bash
# step: D-04-llm-serving
# phase: applications
# requires: D-01-operator
# provides: LLM serving stack (vLLM/LocalAI + LiteLLM) when LLM_SUPPORT is on
# mutates: LLM workloads in platform-kernel

check() {
    # LLM serving off for this cluster: no verdict to give about a stack that
    # was never meant to exist here.
    [[ "${LLM_SUPPORT:-false}" == "true" ]] || return "${CHECK_UNDEFINED}"
    kubectl get deployment -n platform-kernel \
        -l app.kubernetes.io/name=litellm -o name 2>/dev/null | grep -q .
}

apply() {
    install_llm_serving
}

destroy() {
    # Synced by the root ApplicationSet; removed with it in 19 teardown.
    return 0
}
