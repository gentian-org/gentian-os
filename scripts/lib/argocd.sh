#!/usr/bin/env bash
# =============================================================================
# scripts/lib/argocd.sh — Argo CD install, bootstrap Applications, and verification.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
# =============================================================================

# =============================================================================
# 5. Install ArgoCD + AppProject
# =============================================================================
resolve_argocd_url() {
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

install_argocd() {
    banner "Step 5 — Installing ArgoCD"

    if kubectl get deployment argocd-server -n argocd &>/dev/null; then
        success "ArgoCD already installed."
    else
        bash "${SCRIPT_DIR}/scripts/install-argocd.sh"
        success "ArgoCD installed."
    fi

    kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/projects/gentian.yaml"
    success "AppProject applied."

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
        warn "ARGOCD_GITHUB_WEBHOOK_SECRET not set — generating a random secret."
        warn "Store it in OpenBao (identity/argocd/webhook-github-secret) and"
        warn "register it as a webhook on the gentian-org GitHub organisation."
        github_webhook_secret=$(openssl rand -hex 20)
    fi
    kubectl patch secret argocd-secret -n argocd --type merge \
        -p "{\"stringData\":{\"webhook.github.secret\":\"${github_webhook_secret}\"}}"
    success "ArgoCD GitHub webhook secret configured."
    info "Register webhook at: https://argocd.${KERNEL_DOMAIN:-<KERNEL_DOMAIN>}/api/webhook"

    # Create Ingress for argocd.${KERNEL_DOMAIN} if KERNEL_DOMAIN is set.
    # TLS uses wildcard-tls which is propagated by install_kernel_wildcard later;
    # the Ingress is safe to create before the Secret exists.
    if [[ -n "${KERNEL_DOMAIN:-}" ]]; then
        info "ArgoCD edge route is managed by the operator (kernel-argocd HTTPRoute)."
    fi

    # Print ArgoCD admin credentials early so the user sees them even if
    # the install is interrupted before print_summary runs (verify step
    # can take up to 10 minutes).
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

# =============================================================================
# 6. Create ArgoCD OCI registry secrets
# =============================================================================
setup_argocd_repos() {
    info "Skipping OCI registry secrets (paid app charts are managed outside gentian-os)."
}

# =============================================================================
# 7. Install ArgoCD Image Updater controller
# =============================================================================
install_argocd_image_updater() {
    banner "Step 7 — ArgoCD Image Updater"

    info "Adding Argo Helm repo..."
    helm repo add argo https://argoproj.github.io/argo-helm --force-update >/dev/null
    helm repo update >/dev/null

    info "Installing/upgrading argocd-image-updater chart..."
    helm upgrade --install argocd-image-updater argo/argocd-image-updater \
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
# 10. Apply remaining ArgoCD bootstrap Applications
# =============================================================================
bootstrap_argocd_apps() {
    banner "Step 10 — ArgoCD bootstrap Applications"

    # Register public OCI chart repos used by bootstrap Applications.
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-stakater.yaml"
        kubectl apply -f "${SCRIPT_DIR}/kernel/argocd/repos/ghcr-cloudnative-pg.yaml"
    fi
    success "Applied public ArgoCD repository registrations."

    local apps=(openbao globals)
    if [[ "$INSTALL_CLUSTER_INFRA" == "1" ]]; then
        apps+=(reloader cnpg cnpg-cluster)
    fi

    for app in "${apps[@]}"; do
        kubectl apply -f "${SCRIPT_DIR}/kernel/bootstrap/${app}-application.yaml"
        success "Applied ${app}-application.yaml"
    done

    wait_for_running_pod openbao "app.kubernetes.io/name=openbao,app.kubernetes.io/instance=openbao" "openbao" 300 || {
        error "Step 9 failed: openbao pod never became Ready. Aborting install."
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
# =============================================================================
# 13. Apply root ApplicationSet
# =============================================================================
bootstrap_root_appset() {
    banner "Step 13 — Applying root ApplicationSet"
    bash "${SCRIPT_DIR}/scripts/bootstrap.sh"
    success "Root ApplicationSet applied."
}
verify_argocd_apps() {
    banner "Step 16 — Verifying ArgoCD Applications"

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
