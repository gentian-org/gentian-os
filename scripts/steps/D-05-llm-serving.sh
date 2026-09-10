#!/usr/bin/env bash
# step: D-05-llm-serving
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
    #
    # Neither branch this used to check matches anything real: nothing in the
    # repo ever creates a release.helm.crossplane.io for vllm (that OR arm was
    # dead), and kernel/services/llm/chart/templates/vllm.yaml's Deployment is
    # labelled app.kubernetes.io/component=vllm-instance +
    # gentianos.io/vllm-instance=<name>, never app.kubernetes.io/name=vllm. So
    # this reported "not satisfied" on every single pass when LLM_SUPPORT is
    # on, re-running apply() every time — harmless since helm upgrade
    # --install is idempotent, but not verifying anything real either.
    kubectl get deployment -n platform-kernel \
        -l app.kubernetes.io/component=vllm-instance -o name 2>/dev/null | grep -q .
}

apply() {
    install_llm_serving
}

destroy() {
    # NOT synced by the root ApplicationSet — this file's own header says so:
    # Argo cannot project spec.llm.instances into Helm values, which is
    # exactly why apply() renders and installs this release directly instead
    # of leaving it to Argo. The claimed "removed with it in 19 teardown" was
    # never true; a purge left the gentian-llm Helm release (Deployment,
    # Service, GPU time-slicing ConfigMap) behind indefinitely. PVCs are
    # deliberately excluded here: they carry helm.sh/resource-policy: keep
    # (see vllm.yaml) specifically so `helm uninstall` leaves cached model
    # weights in place, the same reasoning prune_stale_vllm_instances already
    # documents for the day-2 case.
    if helm status gentian-llm -n platform-kernel >/dev/null 2>&1; then
        gentian_run helm uninstall gentian-llm -n platform-kernel || true
    fi
}
