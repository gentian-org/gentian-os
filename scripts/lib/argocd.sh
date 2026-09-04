#!/usr/bin/env bash
# =============================================================================
# scripts/lib/argocd.sh — Argo CD install, bootstrap Applications, and verification.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# _apply_argocd_repo_creds <role> <repo_var> <auth_var> <user_var> <token_var>
#
# Registers a prefix-matched ArgoCD repo-creds Secret directly from the
# shell-collected credential — the one Path-A exception in an otherwise
# ESO/OpenBao-managed credential design (see repository-default.yaml). Exists
# only for repositories a bootstrap Application needs before OpenBao is
# reachable; today that is os alone, called from install_argocd() above.
#
# url is a PREFIX match in an ArgoCD repo-creds Secret (secret-type:
# repo-creds, as opposed to the exact-match secret-type: repository the
# Composition emits later) — an exact repo URL here matches only that repo, so
# it does not need trimming or wildcarding.
#
# Skipped, not an error, when auth is "none" (public repo, nothing to
# authenticate) or when the credential was never collected — collect_bootstrap_credentials
# only gathers it when _requirement_applies() gated it in, which mirrors the
# same GENTIAN_OS_AUTH check here.
# =============================================================================
_apply_argocd_repo_creds() {
    local role="$1" repo_var="$2" auth_var="$3" user_var="$4" token_var="$5"
    local repo="${!repo_var:-}"
    local auth="${!auth_var:-none}"
    [[ -n "${repo}" && "${auth}" != "none" ]] || return 0

    local user="${!user_var:-}" token="${!token_var:-}"
    if [[ -z "${token}" ]]; then
        warn "No credential collected for ${role} repository (${auth_var}=${auth}); skipping bootstrap ArgoCD repo-creds Secret."
        return 0
    fi

    info "Registering bootstrap ArgoCD repo-creds for ${role} (${repo})..."
    if [[ "${auth}" == "bearer" ]]; then
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: argocd-repo-creds-bootstrap-${role}
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repo-creds
stringData:
  type: git
  url: ${repo}
  bearerToken: "${token}"
EOF
    else
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: argocd-repo-creds-bootstrap-${role}
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repo-creds
stringData:
  type: git
  url: ${repo}
  username: "${user}"
  password: "${token}"
EOF
    fi
}

# =============================================================================
# 4. Install ArgoCD + AppProject
# =============================================================================
resolve_argocd_url() {
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        echo "https://argocd.${KERNEL_DOMAIN}"
        return 0
    fi
    local ingress_host svc_type node_port lb_host lb_ip

    _pick_node_ip() {
        local detected
        if [[ -n "${NODE_IP:-}" ]]; then
            if _is_testnet_ip "${NODE_IP}"; then
                warn "NODE_IP=${NODE_IP} looks like documentation/testnet IP; auto-detecting real node IP instead." >&2
            else
                echo "${NODE_IP}"
                return 0
            fi
        fi

        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        detected=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)
        if [[ -n "$detected" ]]; then
            echo "$detected"
            return 0
        fi
        echo "<node-ip>"
        return 0
    }

    ingress_host=$(kubectl get ingress -n argocd \
        -o jsonpath='{.items[0].spec.rules[0].host}' 2>/dev/null || true)
    if [[ -n "$ingress_host" ]]; then
        echo "https://${ingress_host}"
        return 0
    fi

    svc_type=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{.spec.type}' 2>/dev/null || true)
    node_port=$(kubectl get svc argocd-server -n argocd \
        -o jsonpath='{range .spec.ports[?(@.name=="https")]}{.nodePort}{end}' 2>/dev/null || true)
    if [[ -z "$node_port" ]]; then
        node_port=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{range .spec.ports[0]}{.nodePort}{end}' 2>/dev/null || true)
    fi

    if [[ "$svc_type" == "LoadBalancer" ]]; then
        lb_host=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
        lb_ip=$(kubectl get svc argocd-server -n argocd \
            -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
        if [[ -n "$lb_host" ]]; then
            echo "https://${lb_host}"
            return 0
        fi
        if [[ -n "$lb_ip" ]]; then
            echo "https://${lb_ip}"
            return 0
        fi
    fi

    if [[ "$svc_type" == "NodePort" && -n "$node_port" ]]; then
        echo "https://$(_pick_node_ip):${node_port}"
        return 0
    fi

    # ClusterIP or unresolved external endpoint.
    echo "kubectl port-forward -n argocd svc/argocd-server 8080:443"
}

# =============================================================================
# tune_argocd_runtime — memory + concurrency settings for the ArgoCD controller
#
# Idempotent and safe to re-run; called on every install.sh run so an existing
# cluster picks these up rather than only freshly-installed ones.
#
# Upstream ships no resources and high concurrency (20 status / 10 operation
# processors), which on a small cluster produces an application-controller that
# OOM-kills itself roughly 20 seconds into every boot — observed here as 48
# restarts across four hours during which nothing in the cluster synced and
# every Application silently sat on a stale revision.
#
# Peak memory tracks CONCURRENCY, not the number of Applications: each processor
# holds the manifests of the app it is comparing. That is why the node still
# showed free memory while the pod was being killed, and why a memory request
# alone did not fix it — the request decides which pod the kernel picks under
# node pressure, and this was not node pressure.
#
# Override per cluster with ARGOCD_STATUS_PROCESSORS / ARGOCD_OPERATION_PROCESSORS
# / ARGOCD_KUBECTL_PARALLELISM; larger clusters can afford the upstream numbers.
# =============================================================================
tune_argocd_runtime() {
    local ns="argocd"

    kubectl -n "${ns}" patch configmap argocd-cmd-params-cm --type merge -p '{"data":{
      "controller.status.processors":"'"${ARGOCD_STATUS_PROCESSORS:-4}"'",
      "controller.operation.processors":"'"${ARGOCD_OPERATION_PROCESSORS:-2}"'",
      "controller.kubectl.parallelism.limit":"'"${ARGOCD_KUBECTL_PARALLELISM:-4}"'"}}' >/dev/null 2>&1       || warn "  Could not patch argocd-cmd-params-cm (absent?)."

    # Requests, not limits. A request lifts the pod out of BestEffort QoS so it
    # is not the kernel's first choice under node pressure; a hard limit would
    # convert an occasional node-level kill into a guaranteed self-inflicted one,
    # because the controller's working set grows with the resources it tracks.
    kubectl -n "${ns}" patch statefulset argocd-application-controller --type=json -p='[
      {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"memory":"768Mi","cpu":"250m"}}}
    ]' >/dev/null 2>&1 || true
    kubectl -n "${ns}" patch deployment argocd-repo-server --type=json -p='[
      {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"memory":"256Mi","cpu":"100m"}}}
    ]' >/dev/null 2>&1 || true

    success "ArgoCD runtime tuned (status=${ARGOCD_STATUS_PROCESSORS:-4} operation=${ARGOCD_OPERATION_PROCESSORS:-2})."
}

# argocd_installed — every artefact install-argocd.sh is responsible for.
#
# Keying "already installed" on the Deployment alone treats a partial apply as a
# finished one. The manifest creates the Deployments before the CRDs, so a
# failure part-way through leaves argocd-server running and applicationsets
# absent — and the step then reports satisfied forever, while the root
# ApplicationSet that needs that CRD fails much later for no visible reason.
argocd_installed() {
    kubectl get deployment argocd-server -n argocd >/dev/null 2>&1 &&
        kubectl get crd applications.argoproj.io >/dev/null 2>&1 &&
        kubectl get crd applicationsets.argoproj.io >/dev/null 2>&1 &&
        # install_argocd applies this AppProject unconditionally on every run,
        # and destroy() (A-09) strips it alongside Applications/ApplicationSets
        # as a first-class, separately-tracked object. Without this, deleting
        # just the AppProject (by hand, or a partial teardown) leaves the
        # server/CRDs satisfying every other check here, so this function
        # keeps reporting installed and install_argocd() never runs again to
        # recreate it — the same shape as the ProviderConfig bug in A-02.
        kubectl get appproject gentian -n argocd >/dev/null 2>&1
}

install_argocd() {
    banner "Installing ArgoCD"

    if argocd_installed; then
        success "ArgoCD already installed."
    else
        # Server-side apply is idempotent, so re-running over a partial install
        # completes it rather than conflicting with it.
        bash "${SCRIPT_DIR}/scripts/bootstrap/install-argocd.sh"
        success "ArgoCD installed."
    fi

    # Runtime tuning is applied on EVERY run, not just first install.
    #
    # install-argocd.sh only executes when argocd-server is absent, so anything
    # set there reaches new clusters and never reaches existing ones. That is
    # exactly wrong for settings that fix a running cluster: the controller
    # OOM-crashloop these values address would have persisted through any number
    # of install.sh re-runs, because the one script that could have fixed it was
    # skipped for being "already installed".
    tune_argocd_runtime

    # Defaults mirror every other call site that threads these through
    # (bootstrap_root_appset, apply_bootstrap_application,
    # apply_gentian_portal_argocd_application) — install_argocd runs at A-09,
    # before any of those, so it cannot assume one of them has already
    # resolved a default into the environment.
    : "${GENTIAN_OS_REPO:=https://github.com/gentian-org/gentian-os}"
    : "${GENTIAN_APPS_REPO:=https://github.com/gentian-org/gentian-apps}"
    : "${GENTIAN_DEPLOYMENTS_REPO:=https://github.com/gentian-org/gentian-deployments}"
    : "${GENTIAN_UI_REPO:=https://github.com/gentian-org/gentian-ui}"
    # Plain bash substitution instead of envsubst: one less required tool
    # (envsubst ships with gettext, not installed by default on macOS), and
    # ${var//search/replace} is literal-string, not regex, so a repo URL
    # containing '/' or any sed-delimiter character needs no escaping on
    # either side. $(<file) and this expansion form are both bash-3.2-safe.
    local _gentian_project
    _gentian_project="$(<"${SCRIPT_DIR}/kernel/argocd/projects/gentian.yaml")"
    _gentian_project="${_gentian_project//\$\{GENTIAN_OS_REPO\}/${GENTIAN_OS_REPO}}"
    _gentian_project="${_gentian_project//\$\{GENTIAN_APPS_REPO\}/${GENTIAN_APPS_REPO}}"
    _gentian_project="${_gentian_project//\$\{GENTIAN_DEPLOYMENTS_REPO\}/${GENTIAN_DEPLOYMENTS_REPO}}"
    _gentian_project="${_gentian_project//\$\{GENTIAN_UI_REPO\}/${GENTIAN_UI_REPO}}"
    printf '%s\n' "${_gentian_project}" | kubectl apply -f -
    success "AppProject applied."

    # os is the one repository ArgoCD must authenticate to before OpenBao
    # exists: B-01-openbao-transit (right after this step) applies the
    # openbao-transit bootstrap Application, and ArgoCD has to pull its chart
    # from osRepo to sync it. Every other credentialed repository (apps, ui,
    # deployments) is first consumed by an Application or ApplicationSet that
    # does not exist until phase B/C, by which point the os-role Repository
    # claim's own ESO-managed Secret has taken over — see
    # scripts/steps/B-XX-os-repository.sh's handoff.
    _apply_argocd_repo_creds os GENTIAN_OS_REPO GENTIAN_OS_AUTH GENTIAN_OS_GIT_USERNAME GENTIAN_OS_GIT_TOKEN

    info "Patching argocd-cm with annotation-based resource tracking..."
    # application.resourceTrackingMethod=annotation prevents ArgoCD from
    # treating Helm-managed resources as part of an ArgoCD app via the
    # default app.kubernetes.io/instance label. Crossplane-managed Helm charts
    # (shared kernel PostgreSQL and MariaDB) stamp every rendered resource with
    #   app.kubernetes.io/instance: <release-name>
    # which equals the ArgoCD Application name. With label-based tracking
    # ArgoCD then "adopts" those Helm-rendered StatefulSets/Services/etc.,
    # finds them missing from git, and PRUNES them seconds after Helm
    # creates them — leaving the Helm release in state=failed with errors
    # like 'services "<release-name>" not found'. Annotation-based
    # tracking uses argocd.argoproj.io/tracking-id and only tracks resources
    # ArgoCD itself applied. See:
    # https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/
    kubectl patch configmap argocd-cm -n argocd --type merge -p '
{
  "data": {
    "application.resourceTrackingMethod": "annotation"
  }
}'
    success "ArgoCD annotation-based resource tracking configured."

    # Treat Pending PVCs as Healthy so ArgoCD sync waves are not blocked by
    # WaitForFirstConsumer PVCs. On a fresh install, a PVC with
    # volumeBindingMode=WaitForFirstConsumer (microk8s-hostpath) stays Pending
    # until a pod mounts it. Without this override ArgoCD considers the PVC
    # Progressing and never advances to the next wave where the consuming pod
    # (or hook job) would be created — causing a permanent deadlock.
    # Lost PVCs are still surfaced as Degraded.
    info "Patching argocd-cm with PVC WaitForFirstConsumer health override..."
    kubectl patch configmap argocd-cm -n argocd --type merge -p '
{
  "data": {
    "resource.customizations.health.PersistentVolumeClaim": "hs = {}\nif obj.status ~= nil then\n  if obj.status.phase == \"Bound\" then\n    hs.status = \"Healthy\"\n    hs.message = \"PVC bound\"\n    return hs\n  end\n  if obj.status.phase == \"Pending\" then\n    hs.status = \"Healthy\"\n    hs.message = \"PVC pending (WaitForFirstConsumer)\"\n    return hs\n  end\n  if obj.status.phase == \"Lost\" then\n    hs.status = \"Degraded\"\n    hs.message = \"PVC lost\"\n    return hs\n  end\nend\nhs.status = \"Progressing\"\nhs.message = \"Waiting for PVC\"\nreturn hs\n"
  }
}'
    success "ArgoCD PVC health override configured."

    # Prevent the ArgoCD application controller from entering a tight
    # reconciliation loop when Crossplane providers continuously update
    # .status on managed Keycloak resources.  Without this, the controller
    # re-enqueues keycloak-config-dev on every Crossplane status write (~20ms),
    # starving all other applications of reconciliation time.
    # resource.ignoreResourceUpdatesEnabled (ArgoCD ≥ 2.10) tells the
    # controller to skip re-queuing an app when only the listed JSON pointers
    # change on the affected resource.
    info "Patching argocd-cm with Crossplane Keycloak resource-update suppression..."
    kubectl patch configmap argocd-cm -n argocd --type merge -p '{
  "data": {
    "resource.ignoreResourceUpdatesEnabled": "true",
    "resource.customizations.ignoreResourceUpdates.client.keycloak.crossplane.io_ProtocolMapper": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_Client": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientDefaultScopes": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientOptionalScopes": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.openidclient.keycloak.crossplane.io_ClientScope": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n",
    "resource.customizations.ignoreResourceUpdates.keycloak.crossplane.io_ProviderConfig": "jsonPointers:\n- /status\n- /metadata/resourceVersion\n- /metadata/generation\n"
  }
}'
    success "ArgoCD Crossplane Keycloak resource-update suppression configured."

    # Diff what a sync would actually change, not what the YAML literally says.
    #
    # A CRD fills in fields nobody wrote. An ExternalSecret declaring a key and a
    # property comes back with five more set; a CNPG Cluster declaring seven
    # fields comes back with forty-three more, twenty-three of them postgres
    # parameters CNPG injects. Argo compared git against the live object and
    # reported OutOfSync forever on applications that were entirely healthy.
    #
    # ServerSideDiff asks the API server what the manifest WOULD become — a
    # dry-run apply — and compares that against live, so defaults and mutating
    # webhooks land on both sides and cancel. It answers "would applying this
    # change anything", which is the question a GitOps tool should be answering,
    # and it needs no per-CRD list of fields to ignore. Kyverno mutates on this
    # cluster, so the webhook half is not hypothetical either.
    #
    # Enumerating the defaulted paths was the alternative and is why this is not
    # one: five for ExternalSecret is maintainable, forty-three for CNPG is not,
    # and a list that silently covers less after each upstream upgrade is the
    # failure mode this platform keeps meeting.
    #
    # managedFieldsManagers does not work here and the reason is worth keeping:
    # it ignores fields a manager OWNS, and these have no field manager at all —
    # argocd-controller owns exactly what git declares, and the defaults are
    # written at admission by nobody.
    #
    # The cost is a global change to how all applications diff, so it was
    # verified as one: enabled, controller restarted, and all 59 applications
    # reached Synced with no controller errors. Reversible by setting this to
    # false and restarting. See docs/architecture.md §"Diffing".
    info "Enabling ArgoCD server-side diff..."
    kubectl patch configmap argocd-cmd-params-cm -n argocd --type merge \
        -p '{"data":{"controller.diff.server.side":"true"}}'
    # The controller reads this at startup, so it needs a restart to take. Safe
    # to run every install: a restart with the value already set is a no-op
    # reconcile, not a change.
    kubectl -n argocd rollout restart statefulset argocd-application-controller >/dev/null 2>&1 || true
    kubectl -n argocd rollout status statefulset argocd-application-controller --timeout=240s >/dev/null 2>&1 || true
    success "ArgoCD server-side diff enabled."

    # Configure ArgoCD server to serve plain HTTP behind the Gateway API edge route.
    # Without this flag ArgoCD redirects HTTP→HTTPS internally and the edge proxy
    # gets into a redirect loop when terminating TLS at the Gateway.
    #
    # reposerver.repo.cache.expiration: how long the repo-server caches both
    # the branch→SHA resolution and the rendered manifest for a (repo, path,
    # revision) tuple.  The default is 24h, which means new commits to a
    # branch are not picked up for up to 24 hours without a webhook push
    # notification.  Setting this to 3m (same as timeout.reconciliation) means
    # every app-controller reconcile cycle triggers a fresh git fetch so new
    # commits are visible within one reconciliation window (~3 minutes).
    # GitHub webhooks further reduce this to near-zero for push events.
    info "Configuring ArgoCD server params (insecure + short repo cache)..."
    kubectl patch configmap argocd-cmd-params-cm -n argocd --type merge \
        -p '{"data":{"server.insecure":"true","reposerver.repo.cache.expiration":"3m"}}'
    kubectl rollout restart deployment argocd-server -n argocd
    kubectl rollout restart deployment argocd-repo-server -n argocd
    kubectl rollout status deployment argocd-server -n argocd --timeout=90s \
        2>/dev/null || true
    kubectl rollout status deployment argocd-repo-server -n argocd --timeout=90s \
        2>/dev/null || true
    success "ArgoCD server running in HTTP mode with 3-minute repo cache."

    # Configure GitHub webhook secret so ArgoCD accepts push notifications from
    # the gentian-org GitHub organisation.  The actual webhook must be registered
    # in GitHub (Settings → Webhooks, or via the GitHub CLI):
    #
    #   URL:     https://argocd.${KERNEL_DOMAIN}/api/webhook
    #   Content-Type: application/json
    #   Secret:  <value from OpenBao: identity/argocd/webhook-github-secret>
    #   Events:  push
    #
    # Without a GitHub webhook ArgoCD still detects new commits within
    # ~3 minutes (via the reduced cache expiry above), but with a webhook
    # syncs happen within seconds of a push.
    local github_webhook_secret="${ARGOCD_GITHUB_WEBHOOK_SECRET:-}"
    if [[ -z "$github_webhook_secret" ]]; then
        # Generate once, then leave it alone.
        #
        # This used to mint a new random secret on every run, so each converge
        # rewrote argocd-secret and invalidated the webhook already registered in
        # GitHub — pushes silently stopped triggering syncs and ArgoCD fell back to
        # its ~3 minute poll, which looks like "GitOps is just slow" rather than a
        # broken webhook.
        local existing
        existing="$(kubectl get secret argocd-secret -n argocd \
            -o jsonpath='{.data.webhook\.github\.secret}' 2>/dev/null || true)"
        if [[ -n "${existing}" ]]; then
            info "Keeping the existing ArgoCD GitHub webhook secret (set ARGOCD_GITHUB_WEBHOOK_SECRET to change it)."
        else
            warn "ARGOCD_GITHUB_WEBHOOK_SECRET not set — generating a random secret once."
            warn "Store it in OpenBao (identity/argocd/webhook-github-secret) and"
            warn "register it as a webhook on the gentian-org GitHub organisation."
            warn "Read it back with: kubectl get secret argocd-secret -n argocd -o jsonpath='{.data.webhook\\.github\\.secret}' | base64 -d"
            github_webhook_secret=$(openssl rand -hex 20)
        fi
    fi
    # Empty only when an existing secret is being kept, which is the one case that
    # must not be overwritten. Everything after this point in the function still
    # runs — an early return here would skip the ArgoCD edge route below.
    if [[ -n "${github_webhook_secret}" ]]; then
        kubectl patch secret argocd-secret -n argocd --type merge \
            -p "{\"stringData\":{\"webhook.github.secret\":\"${github_webhook_secret}\"}}"
        success "ArgoCD GitHub webhook secret configured."
    else
        success "ArgoCD GitHub webhook secret unchanged."
    fi
    info "Register webhook at: https://argocd.${KERNEL_DOMAIN:-<KERNEL_DOMAIN>}/api/webhook"

    # Create Ingress for argocd.${KERNEL_DOMAIN} if KERNEL_DOMAIN is set.
    # TLS uses wildcard-tls which is propagated by install_kernel_wildcard later;
    # the Ingress is safe to create before the Secret exists.
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        info "ArgoCD edge route is managed by the operator (kernel-argocd HTTPRoute)."
    fi

    # Print ArgoCD admin credentials early so the user sees them even if
    # the install is interrupted before the final summary runs (verify
    # step can take up to 10 minutes).
    local argocd_pw argocd_url
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n argocd \
                    -o jsonpath='{.data.password}' 2>/dev/null \
                    | base64 -d 2>/dev/null || echo "")
    argocd_url=$(resolve_argocd_url 2>/dev/null)
    if [[ -n "$argocd_pw" ]]; then
        info "ArgoCD URL   : ${argocd_url}"
        info "ArgoCD login : admin / ${argocd_pw}"
    else
        warn "ArgoCD initial-admin-secret not yet available; will be shown in final summary."
    fi
}

# Configure ArgoCD OIDC settings and group mapping.
configure_argocd_oidc() {
    local kernel_domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN required}"
    info "Configuring ArgoCD OIDC (Keycloak integration)..."

    # 1. Trust the wildcard-tls CA (self-signed or staging issuer support)
    local ca_cert
    ca_cert=$(kubectl get secret wildcard-tls -n argocd -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 -d || true)
    if [[ -z "$ca_cert" ]]; then
        ca_cert=$(kubectl get secret wildcard-tls -n argocd -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d || true)
    fi
    if [[ -n "$ca_cert" ]]; then
        info "Registering gateway CA certificate in argocd-tls-certs-cm..."
        kubectl patch configmap argocd-tls-certs-cm -n argocd --type merge \
            --patch "{\"data\":{\"id.${kernel_domain}\":$(jq -R -s '.' <<<"${ca_cert}")}}"
    fi

    # 2. Patch argocd-cm with OIDC settings and external URL
    local oidc_config
    oidc_config=$(cat <<EOF
name: Keycloak
issuer: https://id.${kernel_domain}/auth/realms/kernel
clientID: gentian-argocd
clientSecret: \$oidc.keycloak.clientSecret
requestedScopes: ["openid", "profile", "email", "groups"]
EOF
)
    kubectl patch configmap argocd-cm -n argocd --type merge -p "
{
  \"data\": {
    \"url\": \"https://argocd.${kernel_domain}\",
    \"oidc.config\": $(jq -R -s '.' <<<"${oidc_config}")
  }
}"

    # 3. Patch argocd-rbac-cm to map group to admin role
    local policy_csv="g, gentian:platform:superadmin, role:admin"
    kubectl patch configmap argocd-rbac-cm -n argocd --type merge -p "
{
  \"data\": {
    \"policy.csv\": $(jq -R -s '.' <<<"${policy_csv}"),
    \"scopes\": \"[groups]\"
  }
}"

    # 4. Restart ArgoCD server to pick up new configurations
    kubectl rollout restart deployment argocd-server -n argocd
    kubectl rollout status deployment argocd-server -n argocd --timeout=90s 2>/dev/null || true
    success "ArgoCD OIDC configuration completed."
}


# =============================================================================
# 4b. Install ArgoCD Image Updater controller
# =============================================================================
install_argocd_image_updater() {
    banner "ArgoCD Image Updater"

    info "Adding Argo Helm repo..."
    helm repo add argo "$(gentian_pin argocd repo)" --force-update >/dev/null
    helm repo update argo >/dev/null

    info "Installing/upgrading argocd-image-updater chart..."
    _helm_retry upgrade --install argocd-image-updater argo/argocd-image-updater \
        --namespace argocd-image-updater \
        --create-namespace \
        --set "config.argocd\.namespace=argocd" \
        --set "config.watch\.namespaces=argocd" \
        --wait \
        --timeout 5m

    success "ArgoCD Image Updater controller is installed persistently in the cluster."
    info "Environment-specific ImageUpdater CRs should be managed in gentian-deployments (GitOps), not in this OS installer."
}

# =============================================================================
# Wait until an Argo CD Application has created its target workload.
# install.sh applies bootstrap Applications with kubectl; the application-
# controller reconciles asynchronously. Polling for pods immediately yields
# permanent "NotScheduledYet" even on a healthy cluster.
# =============================================================================
_wait_for_argocd_application_workload() {
    local app="$1" ns="$2" resource_kind="$3" label_selector="$4" timeout="${5:-300}"
    local start=$SECONDS elapsed=0 sync_status health sync_msg

    info "Waiting for Argo CD Application '${app}' to deploy ${resource_kind} in ${ns} (up to ${timeout}s)..."
    kubectl rollout status statefulset/argocd-application-controller -n argocd \
        --timeout=120s >/dev/null 2>&1 \
        || warn "argocd-application-controller not Ready yet — continuing to poll."

    kubectl annotate application "${app}" -n argocd \
        argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true

    while (( elapsed < timeout )); do
        if kubectl get "${resource_kind}" -n "${ns}" -l "${label_selector}" \
                --no-headers 2>/dev/null | grep -q .; then
            success "Argo CD Application '${app}' created ${resource_kind} in ${ns}."
            return 0
        fi

        sync_status=$(kubectl get application "${app}" -n argocd \
            -o jsonpath='{.status.sync.status}' 2>/dev/null || true)
        health=$(kubectl get application "${app}" -n argocd \
            -o jsonpath='{.status.health.status}' 2>/dev/null || true)
        sync_msg=$(kubectl get application "${app}" -n argocd \
            -o jsonpath='{.status.operationState.message}' 2>/dev/null || true)

        if [[ "${sync_status}" == "Unknown" && -n "${sync_msg}" ]]; then
            error "Argo CD Application '${app}' failed to sync: ${sync_msg}"
            error "Inspect: kubectl describe application ${app} -n argocd"
            return 1
        fi

        if (( elapsed % 30 == 0 )); then
            echo "  [${elapsed}s] app=${app} sync=${sync_status:-<none>} health=${health:-<none>}"
            [[ -n "${sync_msg}" ]] && echo "         message: ${sync_msg}"
        fi
        sleep 5
        elapsed=$((SECONDS - start))
    done

    error "Timed out waiting for Argo CD Application '${app}' to create ${resource_kind} in ${ns}."
    error "Inspect: kubectl describe application ${app} -n argocd"
    kubectl get application "${app}" -n argocd \
        -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status' 2>/dev/null \
        | sed 's/^/  /' || true
    return 1
}
# =============================================================================
# 6. Apply remaining ArgoCD bootstrap Applications
# =============================================================================
bootstrap_argocd_apps() {
    banner "ArgoCD bootstrap Applications"

    # Register the public OCI chart repos the bootstrap Applications pull from.
    #
    # Repository claims rather than hand-written Secrets: the ArgoCD Secret is
    # composed from the claim, so one object describes the repository and one
    # Composition decides what a repository produces.
    #
    # These carry no credential, which the XRepository schema allows — a public
    # registry has nothing to authenticate, and requiring one meant naming a
    # vault path for a secret that does not exist.
    #
    # The claim also whitelists the source in the tenants AppProject, which the
    # Cluster XR creates later. That composed Object cannot sync until then and
    # retries until it can; the repository Secret these Applications actually
    # need is a separate Object and lands immediately.
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-stakater.yaml"
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-cloudnative-pg.yaml"
    fi
    # Unconditional, unlike the chart claims: this covers the gentian-org git
    # sources themselves (gentian-os, gentian-ui, gentian-apps), which every
    # install fetches regardless of cluster-infra. The ExternalSecret syncs
    # once OpenBao holds the deployments credential (B-10); until then Argo CD
    # fetches anonymously, exactly as it always did during bootstrap.
    kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/github-gentian-org-repocreds.yaml"
    success "Applied public chart repository claims."

    local apps=(openbao globals)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        apps+=(reloader cnpg kernel-admin)
        # external-dns was written and then never applied: nothing named it in
        # this list, and the bootstrap chart only renders the templates it is
        # asked for. So the controller that makes tenant hostnames resolve was
        # present in the repository and absent from every cluster, and the gap
        # read as "DNS is managed by hand here" rather than as a missing step.
        #
        # Opt-in, though, because it is the SECOND writer. On a tunnel cluster
        # the operator's edge-DNS adapter already creates the tenant CNAMEs and
        # the tunnel ingress rules; adding external-dns to that is two
        # controllers reconciling one record set. Switching that on as a side
        # effect of a re-run somebody started for another reason is not a
        # decision an installer gets to make — so the claim makes it.
        if [[ "${EXTERNAL_DNS_ENABLED:-false}" == "true" ]]; then
            apps+=(external-dns)
        else
            info "certificates.externalDns is not set; external-dns is not installed."
            info "  DNS records are whatever already writes them here — by hand,"
            info "  or the operator's own adapter."
        fi
    fi

    for app in "${apps[@]}"; do
        # Not "apply, then announce success": apply_bootstrap_application exits
        # on a real failure, but announcing before knowing is how a missing
        # Application read as an applied one.
        apply_bootstrap_application "${app}" || {
            error "Applying bootstrap Application ${app} failed."
            exit 1
        }
        success "Applied bootstrap Application ${app}"
    done

    wait_for_running_pod openbao "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao" "openbao" 300 || {
        error "openbao pod never became Ready. Aborting install."
        exit 1
    }

    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        # ArgoCD applies the Application and then syncs asynchronously, so the
        # Deployments do not exist yet when we return from the apply loop above.
        # Poll until the Deployment appears before calling kubectl wait.

        info "Waiting for reloader deployment to be created by ArgoCD (up to 5 min)..."
        _deadline=$((SECONDS + 300))
        until kubectl get deployment reloader-reloader -n stakater-system &>/dev/null; do
            (( SECONDS < _deadline )) || { error "Timed out waiting for reloader Deployment to appear."; exit 1; }
            sleep 5
        done
        kubectl wait --for=condition=available --timeout=300s \
            deployment/reloader-reloader -n stakater-system
        success "Reloader deployment is available."

        info "Waiting for CNPG operator deployment to be created by ArgoCD (up to 5 min)..."
        _deadline=$((SECONDS + 300))
        until kubectl get deployment cnpg-cloudnative-pg -n cnpg-system &>/dev/null; do
            (( SECONDS < _deadline )) || { error "Timed out waiting for CNPG Deployment to appear."; exit 1; }
            sleep 5
        done
        kubectl wait --for=condition=available --timeout=300s \
            deployment/cnpg-cloudnative-pg -n cnpg-system
        success "CNPG operator deployment is available."
    else
        warn "Cluster infra disabled: skipped reloader/CNPG bootstrap apps."
    fi
}
verify_argocd_apps() {
    banner "Verify — ArgoCD Applications"

    # Restart the application-controller once to clear any stale resource
    # health cached during the OpenBao seal-migration window (when ESO
    # transiently couldn't read secrets). Without this, Applications
    # whose underlying resources are now healthy can stay reported as
    # Degraded indefinitely because ArgoCD doesn't re-evaluate cached
    # resource health unless the resource generation changes.
    info "Restarting argocd-application-controller to clear stale health cache..."
    kubectl rollout restart statefulset -n argocd argocd-application-controller \
        >/dev/null 2>&1 || true
    kubectl rollout status  statefulset -n argocd argocd-application-controller \
        --timeout=120s >/dev/null 2>&1 || warn "application-controller rollout did not become ready in 120s; continuing."

    local timeout=${VERIFY_TIMEOUT:-600}
    local interval=15
    local elapsed=0
    local total synced healthy bad_lines
    info "Waiting up to ${timeout}s for all Applications to become Synced+Healthy..."

    while true; do
        # If no Applications exist yet, keep waiting (root ApplicationSet may
        # still be generating children).
        total=$(kubectl get applications -n argocd --no-headers 2>/dev/null | wc -l)
        if [[ "$total" -eq 0 ]]; then
            if [[ $elapsed -ge $timeout ]]; then
                warn "No ArgoCD Applications appeared within ${timeout}s."
                export VERIFY_STATUS="empty"
                return 1
            fi
            printf "  …no Applications yet (%ds/%ds)\n" "$elapsed" "$timeout"
            sleep "$interval"; elapsed=$((elapsed + interval))
            continue
        fi

        synced=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.sync.status=="Synced")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)
        # Bootstrap operator / ApplicationSet parent: kube-defaulted fields or
        # Argo tracking annotations can leave apps OutOfSync while Healthy.
        while IFS= read -r _app; do
            [[ -n "$_app" ]] && synced=$((synced + 1))
        done < <(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.sync.status=="OutOfSync" && @.status.health.status=="Healthy")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | grep -E '^(gentian-os|gentian-appsets|gentian-portal)$' || true)
        healthy=$(kubectl get applications -n argocd \
            -o jsonpath='{range .items[?(@.status.health.status=="Healthy")]}{.metadata.name}{"\n"}{end}' \
            2>/dev/null | wc -l)

        printf "  apps=%d synced=%d healthy=%d (%ds/%ds)\n" \
            "$total" "$synced" "$healthy" "$elapsed" "$timeout"

        if [[ "$synced" -eq "$total" && "$healthy" -eq "$total" ]]; then
            success "All ${total} ArgoCD Applications are Synced and Healthy."
            export VERIFY_STATUS="ok"
            export VERIFY_TOTAL="$total"
            return 0
        fi

        if [[ $elapsed -ge $timeout ]]; then
            bad_lines=$(kubectl get applications -n argocd \
                -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status' \
                --no-headers 2>/dev/null | awk '$2!="Synced" || $3!="Healthy"')
            warn "Timed out after ${timeout}s with ${total} Applications, ${synced} Synced, ${healthy} Healthy."
            echo "  Degraded / out-of-sync Applications:"
            while IFS= read -r line; do
                [[ -n "$line" ]] && echo "    $line"
            done <<< "$bad_lines"
            export VERIFY_STATUS="degraded"
            export VERIFY_TOTAL="$total"
            export VERIFY_BAD="$bad_lines"
            return 1
        fi

        sleep "$interval"; elapsed=$((elapsed + interval))
    done
}

# =============================================================================
# Summary — portal admin credentials for install output
# =============================================================================
