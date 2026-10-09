#!/usr/bin/env bash
# step: A-06-argocd
# phase: control-plane
# requires: A-01-namespaces
# provides: Argo CD (server, repo-server, application controller, applicationset controller), argocd-image-updater, and, for gentian-os and deployments where they authenticate, the login Argo CD reads them with until the vault supplies it, in the gitops namespace
# mutates: the gitops namespace, Argo CD CRDs, cluster-scoped RBAC, the bootstrap repo-creds Secrets (argocd-repo-creds-bootstrap-gentian-os, argocd-repo-creds-bootstrap-deployments)
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
        # The Deployment, not `helm status`: the driver does not configure
        # helm for check(), and a release that exists while its Deployment
        # does not is a step reporting satisfied for something that is not
        # running. Asking Kubernetes answers the question the step is about.
        # By label, not name: chart 1.x names it argocd-image-updater-controller,
        # and asking for the 0.x name left this check failing forever, so every
        # run re-installed the updater (thirteen Helm revisions on one cluster).
        kubectl get deployment -n "${ns}" -l app.kubernetes.io/name=argocd-image-updater \
            -o name 2>/dev/null | grep -q . &&
        # Serving plain HTTP is not a preference here, it is what makes the
        # console reachable at all; a step that reports satisfied without it
        # leaves a redirect loop nothing else in the sequence looks at.
        [[ "$(kubectl get configmap argocd-cmd-params-cm -n "${ns}" -o jsonpath='{.data.server\.insecure}' 2>/dev/null)" == "true" ]] &&
        # And the diff computed the way the sync applies: see _a06_install.
        [[ "$(kubectl get configmap argocd-cmd-params-cm -n "${ns}" -o jsonpath='{.data.controller\.diff\.server\.side}' 2>/dev/null)" == "true" ]] &&
        [[ "$(kubectl get clusterrolebinding argocd-application-controller -o jsonpath='{.subjects[0].namespace}' 2>/dev/null)" == "${ns}" ]] &&
        _a06_repo_credentials_ok
}

# check()'s last question: a credential for every repository Argo CD reads
# before that repository's claim can hand it one from the vault.
#
# This used to be left out of check(), because the bridge is temporary: C-05
# deletes it once the claim's own Secret holds the login, and a check that
# demanded the bridge would have been unsatisfiable from then on. It asks the
# question the step is for instead -- can Argo CD read the repository -- which
# the claim's Secret answers as well as the bridge does. Left out, a cluster
# whose Argo CD was installed without the credential -- by a release that
# registered none for the deployments repository -- reported this step
# satisfied and never got one.
#
# _a06_argocd_ok asks everything in check() but this, for apply().
_a06_repo_credentials_ok() {
    [[ "${_A06_ARGOCD_ONLY:-0}" != "1" ]] || return 0
    local req
    for req in $(argocd_bridged_repositories); do
        argocd_repo_credential_ok "${req}" || return 1
    done
    return 0
}

_a06_argocd_ok() { _A06_ARGOCD_ONLY=1 check; }

_a06_install() {
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
    # controller.diff.server.side is set with them, because most of what Argo
    # CD syncs here syncs with ServerSideApply=true. Under server-side apply
    # the API server fills a CRD's schema defaults at apply time; Argo CD's
    # default diff is computed client-side and does not know them, so it
    # reports a difference no sync can remove. On the first fresh v5 install
    # nine Applications sat OutOfSync and Healthy that way -- the data-plane
    # engines, the claims, the operator among them -- and C-02 waited fifteen
    # minutes on one whose live object matched git exactly. Server-side diff
    # asks the API server, which is the same question the sync asked.
    #
    # Framing is NOT set here. The console opens Argo CD in a window on the
    # desktop, and what may frame a kernel host is the Gateway's answer for
    # every one of them -- Argo CD's own setting would be a second writer of
    # the same header, and its x-frame-options cannot be turned off through a
    # parameter anyway: an empty value falls back to the default.
    kubectl patch configmap argocd-cmd-params-cm -n "${ns}" --type merge \
        -p '{"data":{"server.insecure":"true","reposerver.repo.cache.expiration":"3m","controller.diff.server.side":"true"}}' >/dev/null
    kubectl rollout restart deployment argocd-server argocd-repo-server -n "${ns}" >/dev/null
    # The application controller is what diffs, and reads its parameters at start.
    kubectl rollout restart statefulset argocd-application-controller -n "${ns}" >/dev/null
    kubectl rollout status statefulset argocd-application-controller -n "${ns}" --timeout=180s >/dev/null
    kubectl rollout status deployment argocd-server -n "${ns}" --timeout=180s >/dev/null
    kubectl rollout status deployment argocd-repo-server -n "${ns}" --timeout=180s >/dev/null
    success "Argo CD serving plain HTTP behind the Gateway, with a 3-minute repo cache and server-side diff."

    banner "Argo CD Image Updater"
    helm repo add argo "$(gentian_pin argocd repo)" --force-update >/dev/null
    helm repo update argo >/dev/null
    _helm_retry upgrade --install argocd-image-updater argo/argocd-image-updater \
        --namespace "${ns}" \
        --set "config.argocd\.namespace=${ns}" \
        --set "config.watch\.namespaces=${ns}" \
        --wait --timeout 5m
}

# The bootstrap bridges: see "The bootstrap repository credentials" in
# scripts/lib/argocd.sh for why these two repositories have one.
#
# A no-op for a repository that does not authenticate (GENTIAN_OS_AUTH=none is
# the default; GENTIAN_DEPLOYMENTS_AUTH=none says the same of a public
# deployments repository), and for one whose Repository claim already supplies
# the login: writing a bridge beside it would only give C-05 something to
# remove again.
_a06_register_repo_credentials() {
    local req name
    for req in $(argocd_bridged_repositories); do
        argocd_repo_needs_credential "${req}" || continue
        name="$(_repo_credential "${req}" vault)"
        if argocd_claim_repo_credential_present "${req}"; then
            info "Argo CD reads the ${name} repository's credential from the vault (repo-${name}); no bootstrap credential is needed."
            continue
        fi
        argocd_bootstrap_repo_credential "${req}"
    done
}

apply() {
    # Argo CD is not installed again for a credential. Its apply restarts the
    # server, the repo-server and the application controller, which is not
    # something to do to a running cluster because a Secret is missing.
    if [[ "${GENTIAN_FORCE:-0}" != "1" ]] && _a06_argocd_ok; then
        info "Argo CD is installed and configured; registering its bootstrap repository credentials only."
    else
        _a06_install
    fi
    _a06_register_repo_credentials
}

# The Applications, removed while their controller still runs.
#
# Argo CD's manifest holds the CRDs and the controller in one file, so
# deleting it takes the controller away while Applications still carry
# finalizers only that controller clears -- resources-finalizer and the
# pre/post-delete hooks. The Application CRD then waits on them for ever, and
# so did the purge. So: delete the ApplicationSets and Applications first,
# give the controller a bounded wait to run their finalizers, and strip what
# it did not get to. The steps before this one have removed what those
# Applications deployed, so there is nothing left for a finalizer to prune.
_a06_release_applications() {
    local ns="$1" kind left deadline
    for kind in applicationsets.argoproj.io applications.argoproj.io; do
        kubectl get "${kind}" -n "${ns}" -o name 2>/dev/null \
            | xargs_r kubectl delete -n "${ns}" --wait=false >/dev/null 2>&1 || true
    done
    deadline=$((SECONDS + 90))
    while (( SECONDS < deadline )); do
        left="$(kubectl get applications.argoproj.io,applicationsets.argoproj.io -n "${ns}" -o name 2>/dev/null || true)"
        [[ -n "${left}" ]] || return 0
        sleep 5
    done
    while IFS= read -r obj; do
        [[ -n "${obj}" ]] || continue
        warn "  ${obj} kept its finalizers; removing them."
        kubectl patch "${obj}" -n "${ns}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    done < <(kubectl get applications.argoproj.io,applicationsets.argoproj.io -n "${ns}" -o name 2>/dev/null || true)
}

destroy() {
    local ns; ns="$(_argocd_ns)"
    _a06_release_applications "${ns}"
    local req
    for req in $(argocd_bridged_repositories); do
        kubectl delete secret "$(argocd_bootstrap_repo_credential_name "${req}")" -n "${ns}" \
            --ignore-not-found >/dev/null 2>&1 || true
    done
    helm uninstall argocd-image-updater -n "${ns}" >/dev/null 2>&1 || true
    curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/$(gentian_pin argocd manifest)/manifests/install.yaml" 2>/dev/null \
        | sed "s/^\(\s*\)namespace: argocd$/\1namespace: ${ns}/" \
        | kubectl delete -n "${ns}" -f - --ignore-not-found --timeout=180s >/dev/null 2>&1 \
        || warn "  Argo CD's manifest did not delete within 3 minutes; the purge continues."
}
