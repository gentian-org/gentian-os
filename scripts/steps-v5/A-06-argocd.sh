#!/usr/bin/env bash
# step: A-06-argocd
# phase: control-plane
# requires: A-01-namespaces
# provides: Argo CD (server, repo-server, application controller, applicationset controller), argocd-image-updater, and the bootstrap repo-creds bridge for a private gentian-os, in the gitops namespace
# mutates: the gitops namespace, Argo CD CRDs, cluster-scoped RBAC, the bootstrap repo-creds Secret
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
        # Serving plain HTTP is not a preference here, it is what makes the
        # console reachable at all; a step that reports satisfied without it
        # leaves a redirect loop nothing else in the sequence looks at.
        [[ "$(kubectl get configmap argocd-cmd-params-cm -n "${ns}" -o jsonpath='{.data.server\.insecure}' 2>/dev/null)" == "true" ]] &&
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

    # The Gateway terminates TLS, so what reaches argocd-server is plain HTTP.
    # In its default mode the server answers that with a redirect to the https
    # URL the browser already asked for, and the browser gives up after a few
    # rounds: the console is unreachable with everything reporting healthy.
    #
    # reposerver.repo.cache.expiration is set with it. The default of 24h
    # caches a branch's resolved SHA and its rendered manifests for a day, so
    # a push lands in the cluster only when someone happens to refresh. Three
    # minutes matches the reconciliation window, and a webhook still shortens
    # it to seconds where one is registered.
    #
    # Framing is NOT set here. The console opens Argo CD in a window on the
    # desktop, and what may frame a kernel host is the Gateway's answer for
    # every one of them -- Argo CD's own setting would be a second writer of
    # the same header, and its x-frame-options cannot be turned off through a
    # parameter anyway: an empty value falls back to the default.
    kubectl patch configmap argocd-cmd-params-cm -n "${ns}" --type merge \
        -p '{"data":{"server.insecure":"true","reposerver.repo.cache.expiration":"3m"}}' >/dev/null
    kubectl rollout restart deployment argocd-server argocd-repo-server -n "${ns}" >/dev/null
    kubectl rollout status deployment argocd-server -n "${ns}" --timeout=180s >/dev/null
    kubectl rollout status deployment argocd-repo-server -n "${ns}" --timeout=180s >/dev/null
    success "Argo CD serving plain HTTP behind the Gateway, with a 3-minute repo cache."

    banner "Argo CD Image Updater"
    helm repo add argo "$(gentian_pin argocd repo)" --force-update >/dev/null
    helm repo update argo >/dev/null
    _helm_retry upgrade --install argocd-image-updater argo/argocd-image-updater \
        --namespace "${ns}" \
        --set "config.argocd\.namespace=${ns}" \
        --set "config.watch\.namespaces=${ns}" \
        --wait --timeout 5m

    # The bootstrap bridge for a private or mirrored gentian-os. B-01's
    # Applications are the first thing to read that repository and they read
    # it before OpenBao is reachable, so the only credential that can serve
    # them is the one the installer collected. A no-op on the public default
    # (GENTIAN_OS_AUTH=none), which is why v5 got this far without it.
    #
    # Deliberately not in check(): the bridge is conditional, and
    # C-06-os-repository-handoff deletes it as soon as the Repository claim
    # proves it can read the same credential from OpenBao. Requiring it here
    # would leave this step unsatisfiable from that moment on.
    _apply_argocd_repo_creds gentian-os GENTIAN_OS_REPO GENTIAN_OS_AUTH \
        GENTIAN_OS_GIT_USERNAME GENTIAN_OS_GIT_TOKEN
}

destroy() {
    local ns; ns="$(_argocd_ns)"
    kubectl delete secret argocd-repo-creds-bootstrap-gentian-os -n "${ns}" \
        --ignore-not-found >/dev/null 2>&1 || true
    helm uninstall argocd-image-updater -n "${ns}" >/dev/null 2>&1 || true
    curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/$(gentian_pin argocd manifest)/manifests/install.yaml" 2>/dev/null \
        | sed "s/^\(\s*\)namespace: argocd$/\1namespace: ${ns}/" \
        | kubectl delete -n "${ns}" -f - --ignore-not-found >/dev/null 2>&1 || true
}
