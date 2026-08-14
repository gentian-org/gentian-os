#!/usr/bin/env bash
# step: 29-llm-serving
# requires: 26-operator
# provides: LLM serving stack (vLLM/LocalAI + LiteLLM) when LLM_SUPPORT is on
# mutates: LLM workloads in platform-kernel

check() {
    [[ "${LLM_SUPPORT:-false}" == "true" ]] || return 0
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
