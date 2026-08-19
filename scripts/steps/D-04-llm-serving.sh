#!/usr/bin/env bash
# step: D-04-llm-serving
# phase: applications
# requires: D-01-operator
# provides: the vLLM GPU release, and LiteLLM tenant/model sync
# mutates: the vLLM Helm release in platform-kernel
#
# LiteLLM, its Postgres and Redis and the mock backend are NOT this step's any
# more — they arrive through the gentian-infra-llm ApplicationSet. What is left
# is the part Argo CD cannot deliver: the vLLM release is rendered from
# spec.llm.instances on the Cluster claim, and Argo cannot project a claim into
# Helm values. That belongs in the Cluster Composition, which reads claims by
# design; until it moves there it is rendered here.

check() {
    # LLM serving off for this cluster: no verdict to give about a stack that
    # was never meant to exist here.
    [[ "${LLM_SUPPORT:-false}" == "true" ]] || return "${CHECK_UNDEFINED}"
    # The vLLM release, which is what this step still owns. LiteLLM is Argo
    # CD's now, so testing for it here would report this step satisfied on the
    # strength of something it does not do.
    kubectl get release.helm.crossplane.io -o name 2>/dev/null | grep -q vllm ||
        kubectl get deployment -n platform-kernel \
            -l app.kubernetes.io/name=vllm -o name 2>/dev/null | grep -q .
}

apply() {
    install_llm_serving
}

destroy() {
    # Synced by the root ApplicationSet; removed with it in 19 teardown.
    return 0
}
