#!/usr/bin/env bash
# step: A-06-argocd
# phase: control-plane
# requires: A-01-namespaces
# provides: Argo CD (server, repo-server, application controller, applicationset controller) and argocd-image-updater in the gitops namespace
# mutates: the gitops namespace, Argo CD CRDs, cluster-scoped RBAC
# pins: argocd

# Argo CD's upstream manifests are written for a namespace called argocd:
# namespaced objects take the namespace from the apply, but the cluster-scoped
# bindings name it in their subjects, so that literal is rewritten to the
# layout's namespace. The image updater is installed beside it, watching the
# same namespace, rather than in one of its own.

_argocd_ns() { ns_kernel gitops; }

check() {
    local ns; ns="$(_argocd_ns)"
    kubectl get crd applicationsets.argoproj.io >/dev/null 2>&1 &&
        kubectl get deployment argocd-server -n "${ns}" >/dev/null 2>&1 &&
        kubectl get deployment argocd-applicationset-controller -n "${ns}" >/dev/null 2>&1 &&
        helm status argocd-image-updater -n "${ns}" >/dev/null 2>&1 &&
        [[ "$(kubectl get clusterrolebinding argocd-application-controller -o jsonpath='{.subjects[0].namespace}' 2>/dev/null)" == "${ns}" ]]
}

apply() {
    banner "Argo CD"
    local ns version
    ns="$(_argocd_ns)"
    version="$(gentian_pin argocd manifest)"
    ns_ensure "${ns}"
    # The upstream manifest names its namespace in the cluster-scoped
    # bindings' subjects ("namespace: argocd"); -n does not reach those, and a
    # controller in another namespace is then bound to nobody. Rewritten to
    # the layout's namespace before applying — the one edit the manifest needs.
    curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/${version}/manifests/install.yaml" \
        | sed "s/^\(\s*\)namespace: argocd$/\1namespace: ${ns}/" \
        | kubectl apply --server-side --force-conflicts -n "${ns}" -f -
    local d
    for d in argocd-server argocd-repo-server argocd-applicationset-controller; do
        kubectl wait --for=condition=available --timeout=300s "deployment/${d}" -n "${ns}"
    done
    kubectl rollout status statefulset/argocd-application-controller -n "${ns}" --timeout=300s

    # No public address: Argo CD is reached through the console's route or a
    # port-forward by a platform administrator, never as a LoadBalancer.
    kubectl patch svc argocd-server -n "${ns}" -p '{"spec":{"type":"ClusterIP"}}' >/dev/null

    banner "Argo CD Image Updater"
    helm repo add argo "$(gentian_pin argocd repo)" --force-update >/dev/null
    helm repo update argo >/dev/null
    _helm_retry upgrade --install argocd-image-updater argo/argocd-image-updater \
        --namespace "${ns}" \
        --set "config.argocd\.namespace=${ns}" \
        --set "config.watch\.namespaces=${ns}" \
        --wait --timeout 5m
}

destroy() {
    local ns; ns="$(_argocd_ns)"
    helm uninstall argocd-image-updater -n "${ns}" >/dev/null 2>&1 || true
    curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/$(gentian_pin argocd manifest)/manifests/install.yaml" 2>/dev/null \
        | sed "s/^\(\s*\)namespace: argocd$/\1namespace: ${ns}/" \
        | kubectl delete -n "${ns}" -f - --ignore-not-found >/dev/null 2>&1 || true
}
