#!/usr/bin/env bash
# =============================================================================
# scripts/lib/bootstrap.sh — bootstrap step bodies
# =============================================================================
# The bodies of the install steps. They lived in install.sh until install.sh became a driver;
# moving them here rather than into each step file keeps Phase 0a a pure restructure, and lets
# Phase 4b delete the ones that go declarative without touching the steps that stay.
# =============================================================================

[[ -n "${GENTIAN_BOOTSTRAP_LOADED:-}" ]] && return 0
GENTIAN_BOOTSTRAP_LOADED=1

# =============================================================================
# =============================================================================
# bootstrap_openbao_for_crossplane — the minimum that must precede Crossplane
# =============================================================================
# Everything this writes is something Crossplane needs in order to manage
# OpenBao at all, and therefore something Crossplane must NOT manage:
#
#   the KV mount      — where its SecretV2 resources write
#   crossplane-write  — the policy its token carries
#   the token Secret  — how provider-vault authenticates
#
# A composition that managed its own authorisation could lock itself out: drift
# or a delete on any of the three leaves provider-vault unable to reconcile the
# resource that would restore it.
#
# Everything else — the Kubernetes auth backend and its config, the eso-read
# policy, both auth roles, and the OIDC write path — is declared by the Cluster
# composition and is deliberately absent here.
# =============================================================================
bootstrap_openbao_for_crossplane() {
    banner "OpenBao bootstrap for Crossplane (mount, policy, token)"

    if ! VAULT_ADDR=$(gentian_service_addr openbao "${OPENBAO_NAMESPACE:-openbao}" 8200 https); then
        error "Could not reach the openbao Service on :8200."
        error "  Neither the ClusterIP nor a kubectl port-forward responded."
        exit 1
    fi
    export VAULT_ADDR
    # bao (unlike a plain curl call) reads its own BAO_ADDR before VAULT_ADDR,
    # so a stale BAO_ADDR left exported in the operator's shell from an
    # earlier manual session — e.g. a pre-purge cluster's address — silently
    # wins here even though VAULT_ADDR just resolved correctly. Found live:
    # this function is the first one in the install to call bao directly, so
    # it is the first to hit that, well before seed_secrets() (later in the
    # run) does its own fresh `export BAO_ADDR=...` and masks the problem for
    # everything after it. Keeping both in sync removes the shell's freedom
    # to disagree with what was just resolved.
    export BAO_ADDR="${VAULT_ADDR}"
    export VAULT_SKIP_VERIFY=true

    if ! _resolve_bao_token; then
        error "Cannot configure the vault without an OpenBao token."
        exit 1
    fi
    export VAULT_TOKEN="${BAO_TOKEN}"

    # ── 1. KV v2 mount — use KV_MOUNT from install.env (default: secret) ─────
    # Must match spec.openbao.kvMount in the cluster claim and the
    # cluster-default Composition, which also uses this env var.
    local _kv_mount="${KV_MOUNT:-secret}"
    if bao secrets list -format=json 2>/dev/null | jq -e --arg m "${_kv_mount}/" '.[($m)]' >/dev/null 2>&1; then
        success "KV v2 mount at '${_kv_mount}/' already present."
    else
        _bao_retry secrets enable -path="${_kv_mount}" kv-v2
        success "KV v2 mount at '${_kv_mount}/' enabled."
    fi

    # ── 2. Kubernetes auth backend ────────────────────────────────────────────

    # ── 3. crossplane-write policy (broad — provider-vault needs sys/* access) ─
    # The Cluster XR Policy MR will keep this policy in sync going forward.
    # _kv_mount is substituted into the heredoc via a quoted-less delimiter so
    # the shell expands the variable before passing the policy to bao.
    #
    # Captured into a variable, not piped straight into _bao_retry, because a
    # heredoc is consumed once: a retry inside _bao_retry would send bao an
    # empty body on the second attempt. _BAO_RETRY_STDIN makes it re-feed this
    # same content fresh on every attempt.
    local _crossplane_write_policy
    _crossplane_write_policy=$(cat <<POLICY
# KV operations
path "${_kv_mount}/data/gentian-os/*"     { capabilities = ["create","read","update","delete"] }
path "${_kv_mount}/metadata/gentian-os/*" { capabilities = ["list","read","delete"] }
# Mount management (SecretMount MR)
path "sys/mounts/*"   { capabilities = ["create","read","update","delete","sudo"] }
path "sys/mounts"     { capabilities = ["read","list"] }
# Policy management (Policy MRs)
path "sys/policies/acl/*" { capabilities = ["create","read","update","delete","list"] }
path "sys/policies/acl"   { capabilities = ["read","list"] }
# Auth method management (Backend/BackendConfig/BackendRole MRs)
path "sys/auth/*"  { capabilities = ["create","read","update","delete","sudo"] }
# list, not just read — the same pair sys/mounts already gets above.
#
# OpenBao filters the sys/auth response by what the token may enumerate, so
# with read alone this token saw a table containing only token/ even though it
# could read sys/auth/oidc directly. provider-vault's AuthBackend finds its
# backend by enumerating that table, so the observe-only oidc AuthBackend in
# the Cluster composition reported "external resource does not exist" against
# a mount that demonstrably existed (accessor and all), stayed unSynced
# forever, and held the whole XCluster at Ready=False until B-08 timed out.
path "sys/auth"    { capabilities = ["read","list"] }
path "auth/+/config"  { capabilities = ["create","read","update"] }
path "auth/+/role/*"  { capabilities = ["create","read","update","delete","list"] }
# Token operations
path "auth/token/create"      { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
POLICY
    )
    _BAO_RETRY_STDIN="${_crossplane_write_policy}" _bao_retry policy write crossplane-write -
    success "crossplane-write policy written."
    # The Kubernetes auth backend, its config, the eso-read policy and both auth
    # roles are NOT written here. The Cluster composition declares all of them,
    # and doing it twice puts two writers on one OpenBao object with no way to
    # see them disagree.
    #
    # None of them is needed to reach the composition: provider-vault
    # authenticates with the static token Secret created below
    # (credentials.source: Secret, provider-configs.yaml), not through the
    # Kubernetes backend. Only ESO needs that, and ESO's ClusterSecretStore is
    # itself composed.

    # ── 5. Mint periodic crossplane token + store as k8s Secret ──────────────
    # provider-vault v3.x (upjet/Terraform-based) does not support
    # InjectedIdentity. It reads credentials from a k8s Secret whose 'credentials'
    # key must contain a JSON object with a 'token' field.
    # Validate existing Secret before re-minting to stay idempotent.
    local need_new_token=1
    if kubectl get secret openbao-crossplane-token -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1; then
        local existing_token
        existing_token=$(kubectl get secret openbao-crossplane-token -n "${CROSSPLANE_NAMESPACE}" \
            -o jsonpath='{.data.credentials}' 2>/dev/null \
            | base64 -d 2>/dev/null \
            | jq -r '.token // empty' 2>/dev/null || true)
        if [[ -n "${existing_token}" ]]; then
            local http_code
            http_code=$(curl -k -s -o /dev/null -w '%{http_code}' --max-time 5 \
                -H "X-Vault-Token: ${existing_token}" \
                "${VAULT_ADDR}/v1/auth/token/lookup-self" 2>/dev/null || echo 000)
            if [[ "${http_code}" == "200" ]]; then
                success "openbao-crossplane-token Secret already valid — skipping."
                need_new_token=0
            else
                info "Existing openbao-crossplane-token is stale (HTTP ${http_code}); recreating."
                kubectl delete secret openbao-crossplane-token \
                    -n "${CROSSPLANE_NAMESPACE}" >/dev/null 2>&1 || true
            fi
        fi
    fi

    if [[ "${need_new_token}" == "1" ]]; then
        info "Minting periodic crossplane-provider token (period=8760h)..."
        local cp_token
        cp_token=$(_bao_retry token create \
            -policy=crossplane-write \
            -period=8760h \
            -orphan \
            -display-name=crossplane-provider \
            -format=json \
            | jq -r '.auth.client_token')
        if [[ -z "${cp_token}" || "${cp_token}" == "null" ]]; then
            error "Failed to mint crossplane-provider token."
            exit 1
        fi
        kubectl create secret generic openbao-crossplane-token \
            -n "${CROSSPLANE_NAMESPACE}" \
            --from-literal=credentials="{\"token\":\"${cp_token}\"}"
        success "openbao-crossplane-token Secret created in ${CROSSPLANE_NAMESPACE}."
    fi

    info "provider-vault ProviderConfig will authenticate via openbao-crossplane-token Secret."

    # Scrub the root token from the process environment so it does not remain
    # visible in /proc/<pid>/environ or child-process env for the rest of
    # the install run.  VAULT_ADDR is kept (harmless; it is a plain URL).
    unset VAULT_TOKEN
}

# =============================================================================
# _derive <context> <purpose> — the derived-credential function, at file scope.
#
# Same derivation as scripts/bootstrap/seed-openbao.sh. There was a third
# implementation, crossplane/functions/derive-secrets/derive.py, deleted as dead
# code in 3920c1ba — derivation happens in shell only.
#
# File scope because it has callers outside the step that first needed it.
# Nested inside create_crossplane_secrets it existed only while B-06 ran, and
# `declare -F _derive` — which _keycloak_smtp_settings tests before deriving the
# Postfix password — was therefore false everywhere else. On every
# MAIL_SERVICE_MODE=system cluster that test failed, so Keycloak realm SMTP was
# skipped with "SMTP credentials incomplete" and the realm could not send an
# invitation or a password reset, while the credentials it needed existed.
# =============================================================================
_derive() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        openssl rand -hex 32
    else
        echo -n "${1}:${2}" | openssl dgst -sha256 \
            -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" | awk '{print $2}'
    fi
}

create_crossplane_secrets() {
    banner "Create derived-credential Secrets for Cluster XR"

    # Enforce minimum-entropy on MASTER_PASSWORD
    if [[ ${#MASTER_PASSWORD} -lt 16 ]]; then
        error "MASTER_PASSWORD is too weak. It must be at least 16 characters long."
        exit 1
    fi

    # Try to read existing master-password and salt from OpenBao
    local existing_secret
    existing_secret=$(bao kv get -mount=secret -format=json gentian-os/kernel/internal/master-password 2>/dev/null || true)
    if [[ -n "${existing_secret}" ]]; then
        local m_val s_val
        m_val=$(echo "${existing_secret}" | jq -r '.data.data.value // empty' 2>/dev/null || true)
        s_val=$(echo "${existing_secret}" | jq -r '.data.data.salt // empty' 2>/dev/null || true)
        if [[ -n "${m_val}" ]]; then
            MASTER_PASSWORD="${m_val}"
        fi
        if [[ -n "${s_val}" ]]; then
            DERIVATION_SALT="${s_val}"
        elif [[ -n "${m_val}" ]]; then
            DERIVATION_SALT=""
        fi
    fi
    if [[ -z "${DERIVATION_SALT:-}" && -z "${existing_secret}" ]]; then
        DERIVATION_SALT=$(openssl rand -hex 16)
    fi
    export DERIVATION_SALT

    # Helper: upsert a K8s Secret in crossplane-system with data.json key.
    #
    # The Secret is what the composition creates the vault path FROM, and it
    # creates it once: an existing path is observed, never overwritten, so a
    # rotated password is never clobbered by a re-run. The cost is that a key
    # added to this file later never arrives on a cluster that already has the
    # path -- which is how a new database's password came to be missing from a
    # path whose Secret carried it, with the failure surfacing as an
    # ExternalSecret that "could not get secret data from provider".
    #
    # So the missing keys are patched in directly, and only the missing ones.
    # Adding what is absent cannot overwrite what somebody rotated.
    _kv_secret() {
        local name="$1" json="$2" path="${3:-}"
        kubectl create secret generic "${name}" \
            -n "${CROSSPLANE_NAMESPACE}" \
            "--from-literal=data.json=${json}" \
            --dry-run=client -o yaml | kubectl apply -f -
        [[ -n "${path}" ]] && _kv_add_missing "${path}" "${json}"
        success "  ${name}"
    }

    # _kv_add_missing <kv path> <json> — add the keys the path does not have.
    # Silent when the vault is unreachable: the path may not exist yet, which
    # is the ordinary case on a first install, and the composition creates it.
    _kv_add_missing() {
        local path="$1" json="$2" have missing
        if ! command -v bao >/dev/null 2>&1; then
            warn "  ${path}: no bao on PATH, so missing keys were not added"
            return 0
        fi
        # The token and the address are both by-products of initialising the
        # vault, and they do not always travel together: a step that has the
        # token from its own init may still have no address, and then every
        # read fails with nothing to say for itself. Both are required.
        if [[ -z "${BAO_TOKEN:-}" || -z "${BAO_ADDR:-}" ]]; then
            OPENBAO_NAMESPACE="${OPENBAO_NAMESPACE:-$(ns_kernel secrets)}" \
                resolve_openbao_access >/dev/null 2>&1 || true
        fi
        if [[ -z "${BAO_TOKEN:-}" ]]; then
            warn "  ${path}: no vault token, so missing keys were not added"
            return 0
        fi
        have=$(bao kv get -mount="${KV_MOUNT:-secret}" -format=json "${path}" 2>/dev/null \
            | jq -c '.data.data // {}' 2>/dev/null) || true
        if [[ -z "${have}" ]]; then
            warn "  ${path}: could not be read, so missing keys were not added"
            return 0
        fi
        missing=$(jq -nc --argjson have "${have}" --argjson want "${json}" \
            '$want | with_entries(select(.key as $k | $have | has($k) | not))')
        [[ "${missing}" == "{}" ]] && return 0
        info "  ${path}: adding $(jq -r 'keys | join(", ")' <<<"${missing}")"
        # One argument per key, built as an array: a value may carry anything
        # openssl produced, and word splitting would cut it in half.
        local -a pairs=()
        while IFS= read -r pair; do pairs+=("${pair}"); done \
            < <(jq -r 'to_entries[] | "\(.key)=\(.value)"' <<<"${missing}")
        bao kv patch -mount="${KV_MOUNT:-secret}" "${path}" "${pairs[@]}" >/dev/null 2>&1 \
            || warn "  ${path}: could not add the missing keys"
    }

    # master-password Secret (referenced by spec.masterPasswordSecretRef in the Cluster claim)
    kubectl create secret generic gentian-os-master-password \
        -n "${CROSSPLANE_NAMESPACE}" \
        --from-literal=password="${MASTER_PASSWORD}" \
        --from-literal=salt="${DERIVATION_SALT}" \
        --dry-run=client -o yaml | kubectl apply -f -
    success "  gentian-os-master-password"

    # ── database/postgresql ───────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-postgresql" \
        "$(jq -nc \
            --arg a "$(_derive postgres postgres_user)" \
            --arg b "$(_derive postgres keycloak_user)" \
            --arg c "$(_derive postgres keycloak_extensions_user)" \
            --arg h "$(_derive postgres openfga_user)" \
            --arg p "$(_derive postgres portal_shell_user)" \
            --arg d "$(_derive postgres director_user)" \
            '{postgres_password:$a,keycloak_user_password:$b,keycloak_extensions_user_password:$c,openfga_user_password:$h,portal_shell_user_password:$p,director_user_password:$d}')" \
        "gentian-os/kernel/database/postgresql"

    # ── database/mariadb ──────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-database-mariadb" \
        "$(jq -nc \
            --arg a "$(_derive mariadb root_password)" \
            '{root_password:$a}')"

    # ── cache/redis ───────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-cache-redis" \
        "$(jq -nc \
            --arg a "$(_derive redis password)" \
            '{auth_password:$a}')"

    # ── storage/minio ─────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-storage-minio" \
        "$(jq -nc \
            --arg a "minio" \
            --arg b "$(_derive minio root_password)" \
            '{root_user:$a,root_password:$b}')"

    # ── identity/keycloak-bootstrap (Suze Keycloak admin password) ─────────────
    _kv_secret "gentian-os-kernel-identity-keycloak-bootstrap" \
        "$(jq -nc \
            --arg a "$(_derive keycloak adminPassword)" \
            '{admin_password:$a}')"

    # ── authz/openfga ─────────────────────────────────────────────────────────
    _kv_secret "gentian-os-kernel-authz-openfga" \
        "$(jq -nc \
            --arg a "$(_derive openfga preshared_key)" \
            '{preshared_key:$a}')"

    # ── mail/postfix (HMAC-derived fields + operator-supplied relay credentials) ─
    # relay_username and relay_password are omitted when unset rather than written
    # as empty strings. An empty string is a value: it made the path complete, the
    # satisfaction probe Ready, and check-credentials report a credential nobody
    # had supplied as satisfied — while Postfix relayed unauthenticated.
    _kv_secret "gentian-os-kernel-mail-postfix" \
        "$(jq -nc \
            --arg host "${EXTERNAL_SMTP_HOST:-}" \
            --arg port "${EXTERNAL_SMTP_PORT:-587}" \
            --arg user "${SMTP_RELAY_USERNAME:-}" \
            --arg pass "${SMTP_RELAY_PASSWORD:-}" \
            '{relay_host:$host,relay_port:$port}
             + (if $user != "" then {relay_username:$user} else {} end)
             + (if $pass != "" then {relay_password:$pass} else {} end)')"

    # ── mail/dovecot (HMAC-derived; only active when MAIL_SERVICE_MODE=system) ─
    # The Cluster XR creates a SecretV2 MR for this path and will seed OpenBao
    # on first apply. The doveadm_password shares its derivation namespace with
    # the minio secret for cross-service derivation consistency.
    _kv_secret "gentian-os-kernel-mail-dovecot" \
        "$(jq -nc \
            --arg doveadm "$(_derive dovecot doveadm_password)" \
            --arg oidc "$(_derive dovecot oidcClientSecret)" \
            '{doveadm_password:$doveadm,oidc_client_secret:$oidc}')"

    # ── oidc/openbao (the Keycloak client secret OpenBao authenticates with) ──
    # Derived rather than operator-supplied: it is shared between two machines,
    # never typed by a human, and so belongs to the generated class. Both ends
    # read it from the same path, so they cannot drift.
    _kv_secret "gentian-os-kernel-oidc-openbao" \
        "$(jq -nc \
            --arg a "$(_derive openbao oidcClientSecret)" \
            '{client_secret:$a}')"

    success "All 10 input Secrets applied to ${CROSSPLANE_NAMESPACE}."
}

# =============================================================================
# report_unready_composed <xr-name> — which composed resources are holding it up
#
# Prints the kind, name and the provider's own message for anything not Ready
# and Synced. The messages are the diagnosis: "path is already in use at oidc/"
# and "ProviderConfig openbao not found" each name their cause exactly.
# =============================================================================
report_unready_composed() {
    local xr_name="$1"
    kubectl get managed -l "crossplane.io/composite=${xr_name}" -o json 2>/dev/null |
        python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for x in doc.get("items", []):
    conds = {c["type"]: c for c in (x.get("status", {}).get("conditions") or [])}
    ready = conds.get("Ready", {}).get("status")
    synced = conds.get("Synced", {}).get("status")
    if ready == "True" and synced == "True":
        continue
    kind = x.get("kind", "?")
    name = x.get("metadata", {}).get("name", "?")
    pols = x.get("spec", {}).get("managementPolicies") or []
    if "keycloak.crossplane.io" in x.get("apiVersion", ""):
        later = "  (later phase — not blocking this step)"
    elif pols == ["Observe"]:
        later = "  (observe-only — not blocking this step)"
    else:
        later = ""
    print(f"    {kind}/{name}  Ready={ready} Synced={synced}{later}")
    msg = (conds.get("Synced", {}).get("message")
           or conds.get("Ready", {}).get("message") or "").strip()
    if msg:
        first = " ".join(msg.split())[:220]
        print(f"      {first}")
'
}

# =============================================================================
# xcluster_structural_ready <xr-name> — is everything this STEP owes ready?
#
# Not the XR's own Ready condition, which function-auto-ready aggregates over
# every composed resource without exception. The Cluster composition also
# renders the Keycloak objects the kernel realm needs — an OIDC Client and its
# two mappers — and those cannot become Ready here by construction: they need
# ProviderConfig.keycloak.crossplane.io, which the root ApplicationSet
# delivers at sync-wave 16, behind Keycloak itself at wave 9. Both are applied
# by C-02, which runs after this step. So waiting on the XR's own Ready
# condition is waiting for a later phase to have already happened, and on a
# genuinely fresh cluster it can only ever time out.
#
# The observe-only resources are excluded for a second, independent reason.
# The composition's jwt AuthBackend is managementPolicies: ["Observe"] — it
# never creates anything, it reads the oidc mount so the tenant-admin policy
# can template the mount accessor. provider-vault reads that backend through
# auth/oidc/config, and at this point in the install there is nothing there to
# read: B-07 enables the mount, but the config is written by
# D-07-openbao-oidc-config, which is four phases later because it needs both
# the Keycloak client secret ESO materialises from a KV path THIS step creates
# and a Keycloak actually serving its discovery document. Waiting here for an
# observe-only resource to reflect a write that a later phase makes is waiting
# for this run to have already finished.
#
# Observe-only is the general form of both cases, and the honest test: a
# resource this composition does not create is a resource this step cannot
# make ready, so it cannot be a gate on this step's own work. Selected by
# managementPolicies rather than by kind or API group, so anything added to
# the composition later under the same contract is covered without editing
# this.
#
# What B-08 owes the steps after it is its `provides:` line — the KV mount and
# its seeded paths, the policies, the auth backends and roles it creates, the
# AppProject and the ClusterSecretStore. All of those are ready in the first
# pass. Nothing here abandons the rest: the Keycloak objects reconcile when
# wave 16 lands, and D-07 writes the oidc config later in this same run.
# =============================================================================
xcluster_structural_ready() {
    local xr_name="$1"
    kubectl get managed -l "crossplane.io/composite=${xr_name}" -o json 2>/dev/null |
        python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(1)
items = doc.get("items", [])
if not items:
    sys.exit(1)
for x in items:
    # Depends on a phase this step precedes (Keycloak, wave 9/16).
    if "keycloak.crossplane.io" in x.get("apiVersion", ""):
        continue
    # The vault OIDC roles need the auth/oidc backend, which is configured
    # once Keycloak answers (D-07 in v4): as Keycloak-dependent as the above.
    if "jwt.vault.upbound.io" in x.get("apiVersion", ""):
        continue
    # The system-tier engines. Their Helm values are ConfigMaps and a Secret
    # that the 08-data-plane ApplicationSet syncs, and that ApplicationSet is
    # created by C-02 -- the step after this one. So a Release cannot be Ready
    # here by construction, for exactly the reason the Keycloak objects above
    # cannot: it is waiting on a phase this step precedes. Gating on it is
    # waiting for a later phase to have already happened, and on a fresh
    # cluster it can only ever time out.
    #
    # C-02 waits for them instead, once it has synced what they read.
    if "helm.crossplane.io" in x.get("apiVersion", ""):
        continue
    # Observe-only: reflects state this composition does not create.
    if [p for p in (x.get("spec", {}).get("managementPolicies") or []) if p == "Observe"] \
       and len(x.get("spec", {}).get("managementPolicies") or []) == 1:
        continue
    conds = {c["type"]: c for c in (x.get("status", {}).get("conditions") or [])}
    if conds.get("Ready", {}).get("status") != "True":
        sys.exit(1)
sys.exit(0)
'
}

# =============================================================================
# composed_permission_errors <xr-name> — composed resources the provider may not touch
#
# Prints one line per composed resource whose provider was refused by the API
# server, and nothing at all otherwise. Used to end a wait early: a permission
# error is not a slow resource, it is a resource that will never arrive.
#
# The distinction matters because both look identical from the XR. An XCluster
# blocked on a missing RBAC rule reports "Unready resources" and stays there for
# the whole timeout, and the sentence naming the missing verb sits on a composed
# object nobody thought to read. Fifteen minutes of waiting, then a message that
# was true in the first thirty seconds.
# =============================================================================
composed_permission_errors() {
    local xr_name="$1"
    kubectl get managed -l "crossplane.io/composite=${xr_name}" -o json 2>/dev/null |
        python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for x in doc.get("items", []):
    conds = {c["type"]: c for c in (x.get("status", {}).get("conditions") or [])}
    msg = " ".join(((conds.get("Synced", {}).get("message") or "")
                    + " " + (conds.get("Ready", {}).get("message") or "")).split())
    # The API server phrases every RBAC denial this way, whatever the verb:
    #   ... is forbidden: User "system:serviceaccount:..." cannot get resource ...
    if "is forbidden" in msg or "cannot list resource" in msg or "cannot get resource" in msg:
        kind = x.get("kind", "?")
        name = x.get("metadata", {}).get("name", "?")
        print(f"    {kind}/{name}")
        print(f"      {msg[:260]}")
'
}

# =============================================================================
# wait_for_xcluster_ready <xr-name> <timeout> — wait, and say what is blocking
#
# A silent wait on a composite is the wrong shape: the XR is not Ready because
# some composed resource is not, and that resource already knows why. Reporting
# only after the deadline means the operator watches a still cursor for fifteen
# minutes and then reads a message that was available in the first thirty
# seconds.
#
# So the not-Ready set is printed periodically. The deadline still ends the
# wait; it just stops being the first moment anything is said.
# =============================================================================
wait_for_xcluster_ready() {
    local xr_name="$1" timeout="$2"
    local secs="${timeout%s}"; secs="${secs%m}"
    case "${timeout}" in *m) secs=$(( secs * 60 )) ;; esac

    local waited=0 interval=15 report_every=60 since_report=0 perm_seen=0
    while (( waited < secs )); do
        # The XR's own Ready first: when everything including the Keycloak
        # objects has reconciled — a re-run on an established cluster — that is
        # the honest answer and the cheapest check.
        if kubectl get "xcluster.gentianos.io/${xr_name}" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
            return 0
        fi
        # Otherwise: is everything this step actually owes ready? On a fresh
        # cluster the Keycloak objects cannot be, and never will be until a
        # later phase this step precedes. See xcluster_structural_ready.
        if xcluster_structural_ready "${xr_name}"; then
            success "Cluster XR ${xr_name}: everything this step provides is Ready."
            info "  Its Keycloak objects reconcile once the root ApplicationSet"
            info "  brings up Keycloak (wave 9) and its ProviderConfig (wave 16)."
            return 0
        fi
        sleep "${interval}"
        waited=$(( waited + interval ))
        since_report=$(( since_report + interval ))
        if (( since_report >= report_every )); then
            since_report=0
            info "  still waiting (${waited}s of ${secs}s) — not Ready:"
            report_unready_composed "${xr_name}"
        fi

        # A permission error does not resolve by waiting. Checked on every poll
        # and required twice in a row, because RBAC that was applied moments ago
        # takes a beat to reach the API server's caches and a single reading
        # would turn that into a false verdict.
        local perm
        perm="$(composed_permission_errors "${xr_name}")"
        if [[ -n "${perm}" ]]; then
            if (( ${perm_seen:-0} )); then
                echo ""
                error "The provider is not permitted to manage these, so waiting cannot help:"
                echo "${perm}" >&2
                error "This is RBAC, not a slow resource. The provider's ServiceAccount is"
                error "  missing a rule for the resource named above."
                error "  Roles: crossplane/providers/provider-rbac.yaml"
                return 1
            fi
            perm_seen=1
        else
            perm_seen=0
        fi
    done

    error "XCluster ${xr_name} did not become Ready within ${timeout}."
    error "Still not Ready:"
    report_unready_composed "${xr_name}"
    error "Diagnose with:"
    error "  kubectl describe xcluster.gentianos.io ${xr_name}"
    error "  kubectl get managed -l crossplane.io/composite=${xr_name}"
    return 1
}

apply_cluster_xr() {
    banner "Apply Cluster XR (kernel structural provisioning)"

    local claims_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/claims"
    [[ -f "${claims_dir}/cluster.yaml" ]] || {
        error "No Cluster claim at ${claims_dir}/cluster.yaml — run install.sh's cluster scaffolding step first."
        exit 1
    }

    # A claim left over from the layout this installer no longer builds is
    # refused before anything is applied. spec.layout is gone from the schema,
    # so a claim still carrying it is a claim nobody has looked at since the
    # kernel moved -- and applying it would place the kernel by a composition
    # that has one answer now, in namespaces the rest of the claim does not
    # describe.
    local claim_layout
    claim_layout="$(_gentian_yq '.spec.layout' "${claims_dir}/cluster.yaml" 2>/dev/null || echo "")"
    if [[ -n "${claim_layout}" && "${claim_layout}" != "v5" && "${claim_layout}" != "null" ]]; then
        error "The Cluster claim says layout '${claim_layout}'. This installer builds one layout and it is not that one."
        error "  Remove spec.layout from ${claims_dir}/cluster.yaml, or scaffold a new claim."
        return 1
    fi
    info "Applying Cluster claim from ${claims_dir}/cluster.yaml..."
    kubectl apply -f "${claims_dir}/cluster.yaml"

    # Crossplane generates a unique name for the XCluster composite (e.g.
    # ifk-l2-prod-k4d2m). Read it from the Claim's resourceRef once populated.
    local claim_name
    claim_name="$(gentian_cluster_claim_name)"
    info "Waiting for Claim ${claim_name} to be bound to a composite (up to 60s)..."
    local xr_name=""
    local deadline=$((SECONDS + 60))
    until [[ -n "${xr_name}" ]]; do
        xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "Claim ${claim_name} was never bound to a composite after 60s."
            error "  kubectl describe cluster.gentianos.io ${claim_name} -n ${CROSSPLANE_NAMESPACE:-crossplane-system}"
            exit 1
        fi
        [[ -n "${xr_name}" ]] || sleep 3
    done
    info "  Composite name: ${xr_name}"

    info "Waiting for XCluster ${xr_name} to be Ready (timeout: ${CLUSTER_XR_TIMEOUT})..."
    wait_for_xcluster_ready "${xr_name}" "${CLUSTER_XR_TIMEOUT}" || exit 1

    success "Cluster XR ${xr_name} is Ready — kernel structural resources provisioned."

    local mr_count
    mr_count=$(kubectl get managed -l "crossplane.io/composite=${xr_name}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    info "  ${mr_count} managed resource(s) reconciled."
}

# =============================================================================
# seed_secrets_remaining — Seed the KV paths that the Cluster XR does not
# manage: internal/master-password, storage/registry, dns/cloudflare,
# database/cnpg, and other kernel paths.
# Delegates to the existing seed-openbao.sh (uses kv_put_once for safety).
# =============================================================================
seed_secrets_remaining() {
    # seed_secrets() is defined in scripts/lib/openbao.sh (sources
    # seed-openbao.sh). The Cluster XR already wrote the HMAC-derived paths (or
    # observed them if they pre-existed). seed_secrets skips those via
    # kv_put_once and writes only the paths the Cluster XR does not cover.
    seed_secrets
    seed_repository_credentials
}

# =============================================================================
# seed_repository_credentials — persist the tier-0 deployments token
# =============================================================================
# The installer collects this token at the credential prompt, and until now the
# only thing it did with it was create a Secret imperatively. Nothing wrote it
# to OpenBao, so the Repository claim in step 16b would gate on a path that
# never gets a value: its ExternalSecrets would never sync, the ArgoCD
# repository credential would never appear, and the gentian-claims
# ApplicationSet would never be able to read the deployments repo.
#
# The path sits under gentian-os/kernel/ because the eso-read policy grants read
# on that prefix and nothing else — a credential stored anywhere else is
# unreadable by ESO no matter who wrote it.
# =============================================================================
seed_repository_credentials() {
    if [[ -z "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}" ]]; then
        info "No deployments repository token supplied; skipping its OpenBao path."
    else
        local username="${GENTIAN_DEPLOYMENTS_GIT_USERNAME:-x-access-token}"
        info "Seeding gentian-os/kernel/repositories/deployments..."
        # kv put, not kv_put_once: a rotated token must actually replace the old one.
        if bao kv put -mount=secret "gentian-os/kernel/repositories/deployments" \
            "username=${username}" \
            "password=${GENTIAN_DEPLOYMENTS_GIT_TOKEN}" >/dev/null 2>&1; then
            success "Deployments repository credential stored."
        else
            error "Could not write the deployments repository credential to OpenBao."
            error "  The Repository claim in step 16b will not become satisfied without it."
            return 1
        fi
    fi

    # os/apps/ui follow the same shape: B-11/B-12/B-13's Repository claims
    # declare a CredentialRequirement against these same paths, and their
    # ExternalSecrets would sit unsatisfied forever without a value here —
    # the same bug this function exists to fix for deployments, one role at
    # a time. Skipped when AUTH is none, which for the public gentian-org
    # default is every install that has not opted into a mirror.
    _seed_one_repo_credential() {
        local role="$1" path="$2" auth_req="$3" user_var="$4" token_var="$5"
        [[ "$(_repo_auth_for "${auth_req}")" != "none" ]] || return 0
        local token="${!token_var:-}"
        [[ -n "${token}" ]] || return 0
        local username="${!user_var:-x-access-token}"
        info "Seeding gentian-os/kernel/repositories/${path}..."
        if bao kv put -mount=secret "gentian-os/kernel/repositories/${path}" \
            "username=${username}" \
            "password=${token}" >/dev/null 2>&1; then
            success "${role} repository credential stored."
        else
            error "Could not write the ${role} repository credential to OpenBao."
            return 1
        fi
    }
    _seed_one_repo_credential "os" "gentian-os" gentian-os-repository \
        GENTIAN_OS_GIT_USERNAME GENTIAN_OS_GIT_TOKEN || return 1
    _seed_one_repo_credential "apps" "gentian-apps" gentian-apps-repository \
        GENTIAN_APPS_GIT_USERNAME GENTIAN_APPS_GIT_TOKEN || return 1
    _seed_one_repo_credential "ui" "gentian-ui" gentian-ui-repository \
        GENTIAN_UI_GIT_USERNAME GENTIAN_UI_GIT_TOKEN || return 1
}

# =============================================================================
# resync_credential_consumers — release the consumers that latched while the
# credentials they read did not exist yet.
#
# The DNS credential is seeded HERE, at B-10, but the two things that read it
# are created well before: the external-dns ExternalSecret arrives with the
# bootstrap chart at B-03, and the DNS-01 ClusterIssuer at A-06. Neither can
# simply be moved later — the ClusterIssuer has to exist before anything
# requests a certificate against it — and the credential cannot be seeded
# earlier, because the KV mount it goes into is B-04's and the policy that
# makes it readable is B-05's. So both consumers necessarily spend several
# steps pointed at a path that answers 403, and both record that failure.
#
# What turns that from a slow install into a stuck one is that neither retries
# on a schedule the install can wait out, once the value does arrive:
#
#   ESO backs off exponentially, per ExternalSecret. Observed on a fresh
#   cluster: retries 128s and then 256s apart, against a refreshInterval of 1h.
#   An ExternalSecret that failed through the B-03 → B-10 window can sit
#   unsynced for a quarter of an hour after its path became readable.
#
#   cert-manager does not re-reconcile an Issuer when the Secret its solver
#   names finally appears. Same cluster, same run: the ClusterIssuer latched
#   `failed to get secret "cloudflare-api-token": not found` at 17:13:15 and
#   was still reporting it at 17:44:03 — seventeen minutes after ESO had
#   materialised that exact Secret in that exact namespace at 17:27:21.
#
# The second one does not clear on its own within any patience the installer
# has, and everything downstream of it fails while naming something else. No
# ready issuer means the kernel wildcard Certificate never issues; the gateway
# goes on serving whatever it already had; and D-07 dies on OpenBao refusing
# the discovery document's TLS — three layers away from the credential that was
# late, and describing a certificate problem rather than a seeding one.
#
# A nudge, not a repair. An annotation is exactly what both controllers watch
# for, neither is asked to do anything it would not eventually have done by
# itself, and a cluster where nothing latched sees no change at all.
# =============================================================================

# The ExternalSecrets that have no Ready=True condition — failed, or never yet
# synced. Both want the same nudge, and distinguishing them here would only
# narrow what the fix covers.
_not_ready_external_secrets() {
    kubectl get externalsecrets.external-secrets.io -A -o json 2>/dev/null |
        jq -r '.items[]
               | select([(.status.conditions // [])[]
                         | select(.type == "Ready" and .status == "True")] | length == 0)
               | "\(.metadata.namespace) \(.metadata.name)"' 2>/dev/null || true
}

# ClusterIssuers only. A-06 creates no namespaced Issuers, and the failure this
# exists for is specifically a cluster-scoped solver reading a Secret out of
# cert-manager's own namespace.
_not_ready_cluster_issuers() {
    kubectl get clusterissuers.cert-manager.io -o json 2>/dev/null |
        jq -r '.items[]
               | select([(.status.conditions // [])[]
                         | select(.type == "Ready" and .status == "True")] | length == 0)
               | .metadata.name' 2>/dev/null || true
}

resync_credential_consumers() {
    local stamp ns name count=0
    stamp="$(date +%s)"

    # ExternalSecrets first, and then a wait: these PRODUCE the Secrets the
    # issuers read, so nudging an issuer before its Secret exists would only
    # latch it a second time — the same ordering fault this function is here to
    # undo, reproduced inside the undoing.
    while read -r ns name; do
        [[ -n "${ns}" && -n "${name}" ]] || continue
        # force-sync is ESO's own trigger: any changed value makes it reconcile
        # now rather than at the end of its backoff.
        if kubectl annotate externalsecret "${name}" -n "${ns}" \
            force-sync="${stamp}" --overwrite >/dev/null 2>&1; then
            count=$(( count + 1 ))
        fi
    done <<< "$(_not_ready_external_secrets)"

    if (( count > 0 )); then
        info "Re-syncing ${count} ExternalSecret(s) that failed before their paths were seeded..."
        # Waited out by PROGRESS, not by emptiness. Emptiness never arrives: the
        # credential catalogue declares a CredentialRequirement for every DNS
        # provider it supports, so a cluster on Cloudflare carries permanently
        # unsynced ExternalSecrets for azuredns, clouddns, hetzner, route53 and
        # the rest — twelve of them on the cluster this was written against.
        # They are not failures, they are providers this cluster does not use,
        # and a wait for "all Ready" would burn its whole budget on every
        # install and then report a timeout for the normal state of affairs.
        #
        # So: stop as soon as a poll produces no further change. What this
        # waits for is the ones that CAN come good doing so, which is all the
        # ClusterIssuer nudge below needs.
        local deadline=$(( SECONDS + 120 )) prev="" now=""
        prev="$(_not_ready_external_secrets)"
        while (( SECONDS < deadline )); do
            sleep 5
            now="$(_not_ready_external_secrets)"
            # `if` for legibility, not for safety: `[[ ... ]] && break` is
            # also correct under `set -e`, because a failing command before the
            # final && of a list is exempt from errexit. The form that does bite
            # is one as the LAST statement of a function, where the function
            # then returns non-zero to a bare caller.
            if [[ "${now}" == "${prev}" ]]; then
                break
            fi
            prev="${now}"
        done
    fi

    count=0
    while read -r name; do
        [[ -n "${name}" ]] || continue
        # cert-manager has no documented force annotation; it watches the
        # Issuer, so any metadata change is the trigger. The annotation is
        # namespaced under gentianos.io rather than cert-manager.io so it can
        # never be mistaken for something the controller itself set.
        if kubectl annotate clusterissuer "${name}" \
            gentianos.io/resynced-at="${stamp}" --overwrite >/dev/null 2>&1; then
            count=$(( count + 1 ))
        fi
    done <<< "$(_not_ready_cluster_issuers)"

    if (( count > 0 )); then
        info "Re-reconciling ${count} ClusterIssuer(s) that latched on an absent credential..."
        # Worth waiting for by name: a ClusterIssuer that is not Ready issues
        # nothing, and the certificate that does not get issued is the kernel
        # wildcard every later step's TLS depends on.
        local deadline=$(( SECONDS + 120 )) stuck
        while (( SECONDS < deadline )); do
            stuck="$(_not_ready_cluster_issuers)"
            if [[ -z "${stuck}" ]]; then
                break
            fi
            sleep 5
        done
        stuck="$(_not_ready_cluster_issuers)"
        if [[ -n "${stuck}" ]]; then
            warn "ClusterIssuer(s) still not Ready after the nudge:"
            printf '%s\n' "${stuck}" | sed 's/^/    /' >&2
            warn "  Certificates naming them stay Pending until they are, and the"
            warn "  gateway keeps serving whatever it already had."
        else
            success "Credential consumers re-synced."
        fi
    fi
}

# =============================================================================
# Print Crossplane-aware installation summary
# =============================================================================
print_summary_cp() {
    local xr_name xr_ready mr_count infra_pg_ready infra_mdb_ready infra_redis_ready infra_minio_ready argocd_url argocd_pw

    # Around a dozen cluster queries, and on a remote API server they add up to
    # the better part of a minute. Announce it: the last thing printed before
    # this was "Bootstrap complete", so silence here reads as a hang at exactly
    # the moment the operator is waiting for their prompt back.
    info "Collecting cluster status for the summary (a dozen queries; this takes a moment)..."

    # Whether this install is actually finished, decided once and used by every
    # claim below.
    #
    # The banner used to read "Bootstrap Complete" unconditionally, directly
    # above a section explaining that handover was NOT finished, under a line
    # claiming phases A–E including handover were done. Three statements on one
    # screen, two of them false, and the false ones in the largest type.
    # Revocation is the last step of the install, so until it has happened the
    # install has not completed and nothing here should say otherwise.
    _gentian_handover_done=""
    if [[ "$(kubectl get configmap gentian-handover \
                -n "${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}" \
                -o jsonpath='{.data.bootstrapCredentialRevoked}' 2>/dev/null)" == "true" ]]; then
        _gentian_handover_done=1
    fi

    local claim_name
    claim_name="$(gentian_cluster_claim_name)"
    xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
    xr_name="${xr_name:-${claim_name}}"

    xr_ready=$(kubectl get "xcluster.gentianos.io/${xr_name}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    # `managed` is a category the providers register; before any provider is
    # installed the query itself fails, and under pipefail that failed the
    # whole summary.
    mr_count=$({ kubectl get managed -l "crossplane.io/composite=${xr_name}" \
        --no-headers 2>/dev/null || true; } | wc -l | tr -d ' ')
    # The system-tier engines. They are composed by the CLUSTER composite, not
    # by a kind of their own: this asked an InfraData claim for them, and after
    # that kind went the four lines read "unknown" on every install -- which
    # looks exactly like four engines that failed to come up.
    #
    # Named <composite>-<chart>, and the composite carries Crossplane's random
    # suffix, so the composite has to be resolved first (xr_name above).
    _release_ready() {
        kubectl get "release.helm.crossplane.io/${xr_name}-$1" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown"
    }
    infra_pg_ready=$(_release_ready postgresql)
    infra_mdb_ready=$(_release_ready mariadb)
    infra_redis_ready=$(_release_ready redis)
    infra_minio_ready=$(_release_ready minio)
    local suze_ready openfga_ready keycloak_ready suze_xr
    suze_ready=$(kubectl get xsuze -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    suze_xr=$(kubectl get suze.gentianos.io "$(gentian_suze_claim_name)" -n "${CROSSPLANE_NAMESPACE:-crossplane-system}" \
        -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || gentian_suze_claim_name)
    # `|| true` on both: grep exits 1 when the release is absent, which is the
    # normal state until phase D deploys Suze. Under pipefail and the ERR trap
    # that ends the run — the summary, whose whole job is to report state, would
    # abort the install for finding a component not deployed yet.
    local openfga_rel keycloak_rel
    openfga_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep openfga | head -1 || true)
    keycloak_rel=$(kubectl get release.helm.crossplane.io -l "crossplane.io/composite=${suze_xr}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep keycloak | head -1 || true)
    openfga_ready=$(kubectl get release.helm.crossplane.io/"${openfga_rel}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")
    keycloak_ready=$(kubectl get release.helm.crossplane.io/"${keycloak_rel}" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "unknown")

    # Resolve these BEFORE the banner to avoid warnings mid-output.
    argocd_url=$(resolve_argocd_url 2>/dev/null)
    # _argocd_ns, not the literal: the gitops namespace is kernel-gitops, and
    # reading the secret from "argocd" printed an empty password on every
    # install -- the one line somebody needs to get in.
    argocd_pw=$(kubectl get secret argocd-initial-admin-secret -n "$(_argocd_ns)" \
        -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)

    echo ""
    echo -e "${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    if [[ -n "${_gentian_handover_done:-}" ]]; then
        echo -e "${CYAN}║     Gentian OS — Install Complete                         ║${NC}"
    else
        echo -e "${YELLOW}║     Gentian OS — Almost There: 1 step left                ║${NC}"
    fi
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${GREEN}  Kernel domain  : ${KERNEL_DOMAIN:-not set}${NC}"
    echo -e "${GREEN}  Tenancy mode   : ${TENANCY_MODE:-multi}${NC}"
    echo -e "${GREEN}  Kernel realm   : ${KERNEL_REALM:-kernel}${NC}"
    echo -e "${GREEN}  Cluster XR     : ${xr_name} (Ready=${xr_ready}, MRs=${mr_count})${NC}"
    echo -e "${GREEN}  System PG      : ${xr_name}-postgresql (Ready=${infra_pg_ready})${NC}"
    echo -e "${GREEN}  System MDB     : ${xr_name}-mariadb (Ready=${infra_mdb_ready})${NC}"
    echo -e "${GREEN}  System Redis   : ${xr_name}-redis (Ready=${infra_redis_ready})${NC}"
    echo -e "${GREEN}  System MinIO   : ${xr_name}-minio (Ready=${infra_minio_ready})${NC}"
    echo -e "${GREEN}  Suze XR       : Ready=${suze_ready} (OpenFGA=${openfga_ready}, Keycloak=${keycloak_ready})${NC}"
    echo ""
    if [[ -n "${_gentian_handover_done:-}" ]]; then
        echo -e "${GREEN}  Completed      : phases A–E (control-plane, secrets, platform, applications, handover)${NC}"
    else
        echo -e "${GREEN}  Completed      : phases A–D (control-plane, secrets, platform, applications)${NC}"
        echo -e "${YELLOW}  Remaining      : handover — see the end of this summary${NC}"
    fi
    # Portal credentials (MASTER_PASSWORD-derived; same as keycloak-portal-bootstrap Job).
    if [[ -f "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh" ]]; then
        # shellcheck source=scripts/lib/portal-login-bootstrap.sh
        source "${SCRIPT_DIR}/scripts/lib/portal-login-bootstrap.sh"
        print_portal_login_summary
    fi
    echo ""
    # The CLI, because tenants are created with it and the installer does not
    # install it. Named here rather than left to the docs: this is the screen an
    # operator has in front of them when they go looking for what to do next.
    echo -e "${GREEN}  Manage tenants and apps with the gentian CLI:${NC}"
    if command -v kubectl-gentian >/dev/null 2>&1; then
        echo -e "${GREEN}    gtnctl tenants deploy <name>     (installed; 'gtnctl version' to check it)${NC}"
    else
        echo -e "${GREEN}    make -C ${SCRIPT_DIR} install-plugin   then: gtnctl tenants deploy <name>${NC}"
    fi
    echo ""
    echo -e "${GREEN}  Inspect authz stack:${NC}"
    echo -e "${GREEN}    kubectl get xsuze,suze -n ${CROSSPLANE_NAMESPACE:-crossplane-system}${NC}"
    echo ""
    echo -e "${GREEN}  Inspect Crossplane managed resources:${NC}"
    echo -e "${GREEN}    kubectl get managed -l crossplane.io/composite=${xr_name}${NC}"
    echo -e "${GREEN}    kubectl get release.helm.crossplane.io | grep ${xr_name}${NC}"
    echo ""
    echo -e "${GREEN}  ArgoCD:${NC}"
    echo -e "${GREEN}    URL  : ${argocd_url}${NC}"
    echo -e "${GREEN}    User : admin${NC}"
    echo -e "${GREEN}    Pass : ${argocd_pw}${NC}"
    # Only while it exists. E-03 deletes it, so naming it afterwards sends the
    # operator to a path that is gone — and on a finished install the answer to
    # "where are the OpenBao tokens" is the recovery kit, not a file in /tmp.
    if [[ -f "${OPENBAO_INIT_FILE}" ]]; then
        echo ""
        echo -e "${GREEN}  OpenBao tokens saved to: ${OPENBAO_INIT_FILE}${NC}"
    fi
    echo ""
    print_handover_summary
    # Before the closing line, not after it: on a finished install this is the
    # only thing still asked of the operator, and "Install Complete" reads as
    # nothing-left-to-do if the ask comes after it.
    report_recovery_kit_left_behind
    if [[ -n "${_gentian_handover_done:-}" ]]; then
        echo -e "${GREEN}  Gentian OS infra bootstrap complete.${NC}"
    else
        echo -e "${YELLOW}  The install is not finished until the steps above are done.${NC}"
    fi
    echo ""
}

# =============================================================================
# print_handover_summary — say that the install is not finished.
#
# The last line of a successful run used to be "bootstrap complete", and an
# operator reasonably stopped reading there. But the cluster still holds a
# bootstrap credential that can write every secret it has, and nothing has yet
# demonstrated that anyone else can — so the remaining work is the part with the
# irreversible step in it, announced at the point where attention still exists.
# =============================================================================
# report_recovery_kit_left_behind — is the kit still where the installer put it?
#
# E-03 writes it beside the checkout because the installer cannot know where
# this operator keeps break-glass material, and says at length that it has to
# be moved. Whether it WAS moved is a fact about the filesystem, so it can be
# checked rather than hoped for — and a kit sitting in a working directory is
# the failure mode the location was a compromise against: it grants every
# derived credential in the cluster to anyone who reads it.
#
# Reported, never acted on. Deleting a file that might be the operator's only
# copy is exactly the wrong reflex, and "moved" is indistinguishable from
# "copied and left" from here.
report_recovery_kit_left_behind() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    local path found=""

    path="$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.recoveryKitPath}' 2>/dev/null || true)"
    if [[ -n "${path}" && -e "${path}" ]]; then
        found="${path}"
    else
        # A cluster whose handover record predates the path being written, or a
        # kit exported by hand under any name. Any .age or .enc in the checkout
        # is treated as one: those are the two extensions --export-recovery-kit
        # produces, they are what .gitignore covers as kits, and nothing else
        # in this repository has either. Only the checkout is looked at —
        # anywhere else is the operator's own filing and none of this
        # function's business.
        local f
        for f in "${SCRIPT_DIR}"/*.age "${SCRIPT_DIR}"/*.enc; do
            [[ -e "${f}" ]] && { found="${f}"; break; }
        done
    fi
    [[ -n "${found}" ]] || return 0

    echo ""
    warn "  RECOVERY KIT IS STILL IN THE WORKING DIRECTORY"
    warn "    ${found}"
    warn "    It grants every derived credential in this cluster to anyone who"
    warn "    can decrypt it. Move it to where your break-glass material lives"
    warn "    — a password manager, a sealed vault, offline media — and delete"
    warn "    this copy once you have checked the moved one opens:"
    warn "      age -d <the-moved-kit> | head -1"
    echo ""
}

print_handover_summary() {
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    local proven revoked kit
    proven="$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.writePathProven}' 2>/dev/null || true)"
    revoked="$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.bootstrapCredentialRevoked}' 2>/dev/null || true)"
    # E-03 gates on BOTH, so this has to name both. It listed only the OIDC
    # sign-in, which is the half an operator can discover by trying it: run
    # E-03 without a kit and it says so. The other half is silent until then,
    # and it is the one with no second chance — the recovery key exists in
    # the init file and nowhere else until a kit is exported.
    kit="$(kubectl get configmap gentian-handover -n "${ns}" \
        -o jsonpath='{.data.recoveryKitExported}' 2>/dev/null || true)"

    if [[ "${revoked}" == "true" ]]; then
        echo -e "${GREEN}  Handover complete — the bootstrap credential is revoked.${NC}"
        echo ""
        return 0
    fi

    # Reached only when the wait in E-03 did not end in a revocation: the
    # operator interrupted it, it timed out, or the run was unattended. So this
    # is short by design — the long explanation was printed while it waited.
    echo -e "${YELLOW}  HANDOVER IS NOT FINISHED${NC}"
    echo -e "${YELLOW}    The installer's credential can still write every secret in this${NC}"
    echo -e "${YELLOW}    cluster, and creating tenants stays held back until it cannot.${NC}"
    echo ""
    if [[ "${kit}" != "true" ]]; then
        # E-02 writes the kit, so this means that step did not run or failed.
        echo -e "${YELLOW}      1. ./install.sh --only E-02      (write the recovery kit)${NC}"
        echo -e "${YELLOW}      2. move the kit somewhere safe${NC}"
        echo -e "${YELLOW}      3. sign in at https://console.${KERNEL_DOMAIN:-<kernel-domain>}/${NC}"
        echo -e "${YELLOW}      4. ./install.sh --only E-03      (revoke and finish)${NC}"
    elif [[ "${proven}" != "true" ]]; then
        echo -e "${YELLOW}      1. move the recovery kit somewhere safe${NC}"
        echo -e "${YELLOW}      2. sign in at https://console.${KERNEL_DOMAIN:-<kernel-domain>}/${NC}"
        echo -e "${YELLOW}      3. ./install.sh --only E-03      (revoke and finish)${NC}"
    else
        echo -e "${YELLOW}    Someone has signed in and a kit exists, so only the revocation${NC}"
        echo -e "${YELLOW}    is left:${NC}"
        echo -e "${YELLOW}      ./install.sh --only E-03${NC}"
    fi
    echo ""
}


# =============================================================================
# scaffold_tenant_deployment — write one tenant's DEFINITION and stop.
#
# The counterpart to scaffold_cluster_deployment, and it stops in the same
# place: it writes the document a human is meant to edit, and nothing that
# deploys it.
#
# A cluster has two directories per tenant and they are not the same thing:
#
#   definitions/tenants/<name>/tenant.yaml
#                                    authored. What the tenant is meant to be.
#   tenants/<name>/                  deployed. What Argo CD syncs, and what the
#                                    operator writes into as apps are installed
#                                    from the store.
#
# They diverge on purpose, so the second is not this script's to create.
# `kubectl gentian tenants deploy <name>` copies the definition across and adds
# the kustomization, and it is also what creates the shared defaults component
# — see ensure_tenant_defaults_component in scripts/kubectl-gentian. This
# function wrote both and gave the component different quotas from the ones
# that command uses, so whichever ran first decided the cluster's tenant sizing.
# =============================================================================
scaffold_tenant_deployment() {
    if [[ ! -d "${GENTIAN_DEPLOYMENTS_PATH}/.git" ]]; then
        error "${GENTIAN_DEPLOYMENTS_PATH} is not a git checkout of gentian-deployments."
        error "  Clone it there first, or point GENTIAN_DEPLOYMENTS_PATH at an existing checkout."
        return 1
    fi

    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:?GENTIAN_DEPLOYMENTS_CLUSTER_ID must be set}"
    local name="${GENTIAN_TENANT_NAME:?GENTIAN_TENANT_NAME must be set}"
    local domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN must be resolved before scaffolding a tenant}"
    local cluster_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}"
    local definition_dir="${cluster_dir}/definitions/tenants/${name}"

    if [[ ! -d "${cluster_dir}/kernel" ]]; then
        error "Cluster ${cluster} has no kernel/ directory in ${GENTIAN_DEPLOYMENTS_PATH}."
        error "  A tenant belongs to a cluster that exists. Install it first:"
        error "    ./install.sh"
        return 1
    fi

    banner "Scaffolding tenant ${name} for cluster ${cluster}"

    if [[ -f "${definition_dir}/tenant.yaml" ]]; then
        warn "clusters/${cluster}/definitions/tenants/${name}/tenant.yaml already exists; leaving it alone."
        _print_tenant_next_steps "${name}" "${cluster}"
        return 0
    fi

    mkdir -p "${definition_dir}"
    {
        printf 'apiVersion: gentianos.io/v1alpha1\n'
        printf 'kind: Tenant\n'
        printf 'metadata:\n'
        printf '  name: %s\n' "${name}"
        printf 'spec:\n'
        printf '  displayName: %s\n' "${GENTIAN_TENANT_DISPLAY_NAME:-${name}}"
        printf '\n'
        printf '  # No adminEmail here. The administrator address is derived:\n'
        printf '  #   admin@%s.%s\n' "${name}" "${domain}"
        printf '  # and it is the Keycloak username too — one identifier, not\n'
        printf '  # two that can disagree. Setting it would point the account at\n'
        printf '  # an address the tenant does not control; this account is\n'
        printf '  # recovered by the cluster administrator, not by mail.\n'
        printf '\n'
        printf '  # Where this tenant is served. Left unset it is %s.%s,\n' "${name}" "${domain}"
        printf '  # which is what a multi-tenant cluster wants. Set it to serve the\n'
        printf '  # tenant on a domain they own instead.\n'
        if [[ -n "${GENTIAN_TENANT_DOMAIN:-}" ]]; then
            printf '  domain: %s\n' "${GENTIAN_TENANT_DOMAIN}"
        else
            printf '  # domain: %s.example.org\n' "${name}"
        fi
        printf '\n'
        printf '  # Prefixes keep one tenant out of another tenant name-space in the\n'
        printf '  # shared data stores. Changing them after provisioning strands what\n'
        printf '  # was created under the old ones.\n'
        printf '  isolation:\n'
        printf '    keycloakRealm: %s\n' "${name}"
        printf '    databasePrefix: %s_\n' "${name//-/_}"
        printf '    s3Prefix: %s-\n' "${name}"
        printf '\n'
        printf '  # Retain keeps the data when the Tenant is deleted; Delete removes it.\n'
        printf '  deletionPolicy: %s\n' "${GENTIAN_TENANT_DELETION_POLICY:-Retain}"
        printf '\n'
        printf '  # Quotas and mail come from this cluster'"'"'s shared tenant-defaults\n'
        printf '  # component, which the deploy command creates. Override here only\n'
        printf '  # what this tenant needs differently from the rest.\n'
        printf '\n'
        printf '  # Apps are installed by profile name from the catalogue.\n'
        printf '  #   kubectl gentian apps list      what this cluster offers\n'
        printf '  #\n'
        printf '  # A profile that is not in the catalogue is refused at admission,\n'
        printf '  # naming the profile — so a typo here fails on deploy, not later.\n'
        printf '  # Everything else is installed from the App Store, which is a tile\n'
        printf '  # on the administrator'"'"'s desktop and not an entry here: the store\n'
        printf '  # runs outside the cluster, and the tile is there whenever the\n'
        printf '  # Cluster claim names one (catalogue.storeUrl).\n'
        printf '  apps:\n'
        printf '  # Subscriptions runs no pods in the tenant: it is a link on the\n'
        printf '  # administrator'"'"'s desktop to billing and entitlements. On by\n'
        printf '  # default, opt-out — delete this entry for a tenant that should not\n'
        printf '  # see it.\n'
        printf '  - profile: gentian-subscriptions-me\n'
        # The claim decides, read from the same file the tenant composition and
        # B-07 read it from. The composition used to inject this app itself,
        # which hid it from the tenant operator — see tenant-default.yaml.
        if [[ "$(yq_get '.spec.llm.enabled' "${cluster_dir}/kernel/claims/cluster.yaml" 2>/dev/null || true)" == "true" ]]; then
            printf '  # AI Chat, listed because this cluster serves LLM\n'
            printf '  # (spec.llm.enabled on the Cluster claim). Delete the entry for a\n'
            printf '  # tenant that should not see it.\n'
            printf '  - profile: open-webui\n'
        fi
        printf '  # Everything else this tenant needs goes beside them, for example:\n'
        printf '  # - profile: nextcloud-base-ce\n'
        printf '  #   addons:\n'
        printf '  #   - nextcloud-calendar-ce\n'
    } >"${definition_dir}/tenant.yaml"

    success "Wrote clusters/${cluster}/definitions/tenants/${name}/tenant.yaml"
    _print_tenant_next_steps "${name}" "${cluster}"
}

_print_tenant_next_steps() {
    local name="$1" cluster="$2"
    local def="clusters/${cluster}/definitions/tenants/${name}/tenant.yaml"
    echo ""
    info "This is the definition only. Nothing is deployed and nothing is committed."
    info "  1. Choose its apps:"
    info "       \$EDITOR ${GENTIAN_DEPLOYMENTS_PATH}/${def}"
    info "  2. Deploy it:"
    info "       kubectl gentian tenants deploy ${name}"
    info "     which copies the definition into clusters/${cluster}/tenants/${name}/,"
    info "     commits and pushes it, and lets Argo CD create the Tenant."
    info "  3. Watch it arrive:  kubectl get tenant ${name} -w"
    echo ""
    info "If the deploy reports the tenant was refused because handover is not"
    info "finished, sign in and open Admin Console → Credentials first — see"
    info "GETTING-STARTED.md, 'Hand the cluster over'."
}


# =============================================================================
# scaffold_cluster_deployment — write this cluster's kernel/ directory in
# gentian-deployments: claims/{cluster,infra-data,suze}.yaml and values.yaml,
# generated from KERNEL_DOMAIN and GENTIAN_DEPLOYMENTS_STAGE. Reached through
# step 0 of `install.sh`.
#
# Per-file checks, not a directory-level one: an existing file is never
# overwritten, so re-running converges a partially-written directory and
# preserves every hand edit made to one that is already complete.
#
# Writing is all it does. The files are left in the working tree for the
# operator to read, edit and commit, because this directory is what the cluster
# is — a generated claim pushed unread is a cluster configured by whoever ran
# the installer rather than by anyone who reviewed it, and the repository is
# shared with every other cluster.
#
# The gentian-os/gentian-portal Applications and the ImageUpdater CR are
# NOT scaffolded here — they're rendered from kernel/bootstrap/chart by
# install_gentian_os_operator()/install_portal_login() (catalogue.sh /
# portal-login-bootstrap.sh) and applied straight to the cluster, never
# committed to gentian-deployments. Their content varies only by the cluster
# and stage the chart is rendered with, so there's nothing cluster-specific
# worth persisting as a file — see docs/deployment.md §3.1.
# =============================================================================
# The cluster's settings, written into the claim explicitly.
#
# Every field the installer later reads is emitted, even where it equals the
# XRD's default. A complete claim means the default path is a fallback for old
# clusters rather than the norm — and it means a reviewer can see what a cluster
# is from the claim alone, instead of inferring it from what is absent.
#
# Empty values are omitted rather than written blank: an empty string is a value
# in YAML and would override the XRD default with nothing.
_claim_cluster_fields() {
    # Emits the whole spec below kernelDomain, and emits EVERY field that
    # decides how the cluster behaves — set, or commented with its default and
    # the reason it is not set.
    #
    # The alternative, writing only what the environment happened to carry,
    # produced a claim that was correct and unreadable: a reader could not tell
    # a deliberate default from a forgotten setting without opening the XRD, and
    # a field that does nothing in this configuration looked the same as one
    # that does. Both questions are answered here, in the file the operator
    # actually edits.
    local nm="${NETWORK_MODE:-tunnel}"
    local mm
    mm="$(gentian_mail_service_mode)"
    local im="${CERT_ISSUER_MODE:-acme-dns01}"
    local pf="${PLATFORM:-}"
    local dp="${DNS_PROVIDER:-cloudflare}"
    local st="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"

    printf '\n'
    printf '  # Where this cluster runs. Selects the edge load-balancer settings\n'
    printf '  # its provider needs; see kernel/platforms.yaml for the full list.\n'
    printf '  #   self-hosted  bare metal or a VM you own, addressed by MetalLB\n'
    printf '  #   openstack / infomaniak / hetzner / aws / gcp / azure\n'
    printf '  #   none         no load-balancer integration at all\n'
    printf '  # Left unset it is detected from the nodes providerID.\n'
    if [[ -n "${pf}" ]]; then
        printf '  platform: %s\n' "${pf}"
    else
        printf '  # platform:                    detected from the nodes\n'
    fi
    if [[ -n "${PLATFORM_PARAMS:-}" ]]; then
        printf '  platformParams:\n'
        printf '%s\n' "${PLATFORM_PARAMS}" | tr ',' '\n' | while IFS= read -r _p; do
            [[ -n "${_p}" && "${_p}" == *=* ]] || continue
            printf '    %s: "%s"\n' "${_p%%=*}" "${_p#*=}"
        done
    else
        printf '  # platformParams:              hetzner needs location; azure a\n'
        printf '  #                              publicIpResourceGroup\n'
    fi

    printf '\n'
    printf '  # How traffic reaches this cluster.\n'
    printf '  #   tunnel     behind a reverse proxy or tunnel; nodeIp is not used\n'
    printf '  #   static-ip  DNS points straight at nodeIp, which is then required\n'
    printf '  networkMode: %s\n' "${nm}"
    if [[ "${nm}" == "static-ip" ]]; then
        printf '  nodeIp: %s\n' "${NODE_IP:-}"
    else
        printf '  # nodeIp:                      not used while networkMode is tunnel\n'
    fi
    if [[ -n "${EDGE_ADDRESS_REF:-}" ]]; then
        printf '  addressRef: %s\n' "${EDGE_ADDRESS_REF}"
    else
        printf '  # addressRef:                  AWS eipalloc ids or an Azure public\n'
        printf '  #                              IP name; neither takes an address\n'
    fi

    printf '\n'
    printf '  # Who issues TLS certificates, and who hosts the zone. Two questions:\n'
    printf '  # how control is proved, and by whom.\n'
    printf '  #   acme-dns01   Lets Encrypt over DNS-01; the only path to a wildcard\n'
    printf '  #   acme-http01  Lets Encrypt over HTTP-01; needs port 80 reachable\n'
    printf '  #   private-ca   your own CA, supplied as certificates.caBundleSecretRef\n'
    printf '  #   self-signed  no public DNS and no ACME reachability; browsers warn\n'
    printf '  certificates:\n'
    printf '    issuerMode: %s\n' "${im}"
    if [[ "${im}" == acme-* ]]; then
        # Asked by prompt_cluster_settings; staging on dev unless answered
        # otherwise. A dev cluster is rebuilt often and Let's Encrypt allows
        # five duplicate certificates per name per week, which one bad
        # afternoon exhausts — and the rate limit is per name, so it outlives
        # the cluster that spent it.
        local ae="${ACME_ENV:-}"
        if [[ -z "${ae}" ]]; then
            ae=production
            [[ "${st}" == "dev" ]] && ae=staging
        fi
        if [[ "${ae}" == "staging" ]]; then
            printf '    # staging: untrusted certificates, generous rate limits.\n'
            printf '    # Switch to production once the names are settled.\n'
        fi
        printf '    acmeEnv: %s\n' "${ae}"
    fi
    if [[ "${im}" == "acme-dns01" ]]; then
        printf '    # cloudflare, route53, clouddns, azuredns, rfc2136, hetzner,\n'
        printf '    # infomaniak — independent of platform above.\n'
        printf '    dnsProvider: %s\n' "${dp}"
        if [[ -n "${DNS_PARAMS:-}" ]]; then
            printf '    dnsParams:\n'
            printf '%s\n' "${DNS_PARAMS}" | tr ',' '\n' | while IFS= read -r _p; do
                [[ -n "${_p}" && "${_p}" == *=* ]] || continue
                printf '      %s: "%s"\n' "${_p%%=*}" "${_p#*=}"
            done
        else
            printf '    # dnsParams:                route53 needs region and\n'
            printf '    #                           hostedZoneID; clouddns a project\n'
        fi
        # On by default where there is a fixed address to publish.
        #
        # A cluster with networkMode: static-ip has one stable IP and a named
        # dnsProvider, which is the whole input external-dns needs — so writing
        # the records is the obvious behaviour and leaving it off produces a
        # claim that contradicts itself: a DNS provider named, an address to
        # point at, and nothing to write anything.
        #
        # That contradiction was invisible while clusters ran on domains whose
        # records had been created by hand. The first install onto a fresh
        # domain sat waiting for names nothing was publishing, and the wait
        # blamed the OIDC discovery URL.
        #
        # A tunnel cluster is genuinely different and stays commented: it has no
        # address to publish, its hostnames resolve through the tunnel's own
        # CNAMEs, and the operator writes the tenant records itself.
        if [[ "${nm}" == "static-ip" ]]; then
            printf '    # external-dns writes this zone from the Gateway hostnames.\n'
            printf '    # Set false where something else already owns these records.\n'
            printf '    externalDns: true\n'
        else
            printf '    # externalDns: true         let external-dns write this zone.\n'
            printf '    #                           Off while networkMode is tunnel: there\n'
            printf '    #                           is no fixed address to publish, and the\n'
            printf '    #                           operator writes the tenant records\n'
            printf '    #                           through the tunnel CNAMEs itself.\n'
        fi
    else
        printf '    # dnsProvider:              only read when issuerMode is acme-dns01\n'
    fi
    if [[ "${im}" == "private-ca" ]]; then
        printf '    caBundleSecretRef:\n'
        printf '      name: %s\n' "${CA_BUNDLE_SECRET_NAME:-gentian-root-ca-tls}"
        printf '      namespace: %s\n' "${CA_BUNDLE_SECRET_NAMESPACE:-cert-manager}"
    else
        printf '    # caBundleSecretRef:         only read when issuerMode is private-ca\n'
    fi

    printf '\n'
    printf '  # Where mail goes.\n'
    printf '  #   external  relay through an SMTP provider; supply the smtp-relay\n'
    printf '  #             credential to the credential manager after install\n'
    printf '  #   system    in-cluster Postfix/Dovecot; requires networkMode static-ip\n'
    printf '  mail:\n'
    printf '    serviceMode: %s\n' "${mm}"
    if [[ "${mm}" == "external" ]]; then
        if [[ -n "${EXTERNAL_SMTP_HOST:-}" ]]; then
            printf '    host: %s\n' "${EXTERNAL_SMTP_HOST}"
        else
            printf '    # host:                    the relay address; set before mail will send\n'
        fi
        printf '    # port: 587                 defaults to 587\n'
        printf '    # starttls: true            defaults to true\n'
    else
        printf '    # host:                     not used while serviceMode is system\n'
    fi

    # egressHost, in both modes, because load_deployments_cluster_settings reads
    # it back (claim_setting MAIL_EGRESS_HOST mail.egressHost) and nothing else
    # writes it. Omitting it entirely is what made this worth emitting: a
    # kernel-mail cluster scaffolded without it gets the operator's fallback SPF
    # record, "v=spf1 mx ~all", which names the INBOUND load balancer — an
    # address that never sends — so SPF fails by construction while reading as
    # plausible. mail_reconciler.go says so; the record only becomes correct
    # once this names the address outbound mail actually leaves from.
    #
    # Commented rather than guessed when unset: it has to agree with a floating
    # IP, a PTR record and an A record that are not in this repo, so a value the
    # scaffold invented would be wrong in a way that looks configured.
    if [[ -n "${MAIL_EGRESS_HOST:-}" ]]; then
        printf '    egressHost: %s\n' "${MAIL_EGRESS_HOST}"
    elif [[ "${mm}" == "system" ]]; then
        printf '    # egressHost:               the name outbound mail leaves from, e.g.\n'
        printf '    #                           mail-egress.%s — required for SPF to pass.\n' "${KERNEL_DOMAIN:-example.com}"
        printf '    #                           Needs a PTR back to it and an A record to the\n'
        printf '    #                           sending address; without it the SPF record\n'
        printf '    #                           names the inbound load balancer and fails.\n'
    else
        printf '    # egressHost:               only for a cluster that sends from its own\n'
        printf '    #                           address rather than through the relay above\n'
    fi

    printf '\n'
    printf '  # Defaults below are in effect. Uncomment a line to change it.\n'
    _claim_default_line tenancyMode  "${TENANCY_MODE:-}"  multi   'one subdomain and Keycloak realm per tenant; single = one tenant owns the cluster'
    _claim_default_line secretMode   "${SECRET_MODE:-}"   derived 'every kernel secret reproducible from the master password; random = independent'
    _claim_default_line routingMode  "${ROUTING_MODE:-}"  gateway 'Envoy Gateway plus the Gateway API; the only supported value'
    _claim_default_line storageClass "${STORAGE_CLASS:-}" ''      'empty means the clusters default StorageClass'

    printf '\n'
    printf '  # Where the backup private key lives. On by default: it goes to OpenBao as\n'
    printf '  # well as the recovery kit, so a cluster administrator can restore without\n'
    printf '  # the kit -- and anyone who reaches OpenBao as one can read every bundle.\n'
    printf '  # Set false to keep it in the kit alone: nothing the cluster holds can then\n'
    printf '  # open a bundle, and losing every copy of the kit loses every backup.\n'
    if [[ "${BACKUP_ESCROW_IDENTITY:-true}" == "false" ]]; then
        printf '  backup:\n'
        printf '    escrowIdentity: false\n'
    else
        printf '  # backup:\n'
        printf '  #   escrowIdentity: true\n'
    fi

    if [[ "${LLM_SUPPORT:-false}" == "true" ]]; then
        printf '  llm:\n'
        printf '    enabled: true\n'
        printf '    gpuAcceleration: %s\n' "${GPU_ACCELERATION:-false}"
        if [[ -n "${GPU_TIME_SLICE_REPLICAS:-}" ]]; then
            printf '    gpuTimeSliceReplicas: %s\n' "${GPU_TIME_SLICE_REPLICAS}"
        else
            printf '    # gpuTimeSliceReplicas: 1   workloads sharing one physical GPU\n'
        fi
        printf '    # The models this cluster serves. Removing an entry removes its\n'
        printf '    # workload; the cached weights survive, so re-adding the same\n'
        printf '    # name does not download tens of gigabytes again.\n'
        printf '    instances: []\n'
        printf '    #  - name: qwen\n'
        printf '    #    modelId: Qwen/Qwen2.5-7B-Instruct\n'
        printf '    #    gpuMemoryUtilization: "0.85"   fraction of GPU memory\n'
        printf '    #    maxModelLen: "8192"            context window, tokens\n'
        printf '    #    modelCacheSize: 60Gi           PVC for the weights\n'
        printf '    #    imageTag: latest               vLLM image tag\n'
        printf '    #    toolCallParser: hermes         empty disables tool calling\n'
        printf '    # External OpenAI-compatible providers, routed through the same\n'
        printf '    # gateway. Independent of gpuAcceleration -- a cluster with no GPU\n'
        printf '    # and no instances serves these and nothing else. Adding an entry\n'
        printf '    # registers its models; removing one deregisters them.\n'
        printf '    providers: []\n'
        printf '    #  - name: infomaniak\n'
        printf '    #    displayName: Infomaniak AI Services\n'
        printf '    #    # Up to and including the version segment. The number is the\n'
        printf '    #    # AI product id (GET /1/ai returns it), not an account id.\n'
        printf '    #    apiBase: https://api.infomaniak.com/2/ai/<product-id>/openai/v1\n'
        printf '    #    # A property of the llm-provider-<name> credential, supplied\n'
        printf '    #    # in the Admin Console. Never the token itself.\n'
        printf '    #    apiKeyProperty: infomaniak_api_key\n'
        printf '    #    models:\n'
        printf '    #      - name: gemma-4-31b              offered as infomaniak/gemma-4-31b\n'
        printf '    #        model: google/gemma-4-31B-it   the id the provider expects\n'
        printf '    #        maxTokens: 8192                omitted leaves LiteLLM guessing\n'
        printf '    #        mode: chat                     embedding models must say so\n'
    else
        printf '  # llm:\n'
        printf '  #   enabled: false            set true on a cluster that serves models\n'
        printf '  #   gpuAcceleration: false    set true when the cluster has GPUs\n'
        printf '  #   gpuTimeSliceReplicas: 1   workloads sharing one physical GPU\n'
        printf '  #   instances: []             the models to serve; see the XRD for fields\n'
    fi

    # Human write access to OpenBao, set rather than left to the operator.
    #
    # Its presence is what creates the auth backend: the composition gates the
    # Keycloak client, the client Secret and the OpenBao policies on
    # oidc.discoveryUrl, B-07 enables the mount, D-07 writes the config. Leave it
    # out and none of that exists — which the XRD describes exactly, and which
    # ends with "no day-2 writes at all".
    #
    # That is not a configuration a cluster should reach by default. The install
    # revokes its own bootstrap token at E-03, and refuses to when nothing else
    # can write — so a claim without this block produces an install that cannot
    # finish its last step. Observed: the operator was asked to sign in, did, and
    # waited on a record that nothing existed to write.
    #
    # The URL is derived rather than asked for, because every part of it is
    # already known: Keycloak is at id.<kernelDomain>/auth and the realm is the
    # oidc.realm default. Anyone who needs a different issuer edits one line.
    printf '\n'
    printf '  # Human write access to OpenBao, federated from Keycloak.\n'
    printf '  # discoveryUrl is what creates the backend; without it the only\n'
    printf '  # write path is the installer bootstrap token, which E-03 revokes.\n'
    printf '  oidc:\n'
    printf '    discoveryUrl: https://id.%s/auth/realms/kernel\n' "${KERNEL_DOMAIN:-<kernel-domain>}"
    printf '    # clientId:          openbao\n'
    printf '    # clientSecretRef:   openbao-oidc-client   Secret in the OpenBao namespace\n'
    printf '    # clusterAdminGroup: /gentian:platform:admin\n'
    printf '    # externalUrl:                             OpenBao UI callback, if exposed\n'

    # Where software may enter this cluster (AD-14).
    #
    # Commented rather than set, because a cluster with no source materialises
    # nothing on reference and that is a working cluster: its profiles arrive
    # with the kernel. Adding a source is a deliberate act, and having it be an
    # edit to this file rather than an environment variable on the director is
    # the point -- opening a catalogue to a tenant is then a commit with an
    # author and a date, and "what may this cluster install from" is answerable
    # without cluster access.
    printf '\n'
    printf '  # Catalogues this cluster may fetch profiles from. A tenant installing\n'
    printf '  # "main/nextcloud-base-ce" gets the bundle from the source named main,\n'
    printf '  # at the digest the App Store stated -- the source itself is not trusted.\n'
    printf '  #\n'
    printf '  #   access: entitled   the store decides, per tenant, with a signed grant\n'
    printf '  #   access: open       your own repository; the tenants listed here may\n'
    printf '  #                      install from it with no grant. Nothing is open by\n'
    printf '  #                      default, and removing a tenant closes it again.\n'
    printf '  # catalogue:\n'
    printf '  #   # Where people are sent for everything the cluster does not\n'
    printf '  #   # list itself: the maintained (me) and licensed (ee) editions.\n'
    printf '  #   # A cluster lists only ce and pe from its own sources.\n'
    printf '  #   storeUrl: https://gentian.org/apps\n'
    printf '  #   sources:\n'
    printf '  #     - name: main\n'
    printf '  #       url: https://store.gentian.org/catalogue\n'
    printf '  #       access: entitled\n'
    printf '  #     - name: in-house\n'
    printf '  #       url: https://git.example.com/profiles\n'
    printf '  #       access: open\n'
    printf '  #       tenants: [demo]\n'
    return 0
}

# _claim_default_line <field> <value> <default> <explanation>
#
# Sets the field when the operator chose something, and otherwise records the
# default that is active. Either way the field appears, so the file lists the
# cluster's whole configuration rather than the part someone happened to set.
_claim_default_line() {
    local field="$1" value="$2" default="$3" why="$4"
    if [[ -n "${value}" && "${value}" != "${default}" ]]; then
        printf '  %s: %s\n' "${field}" "${value}"
    else
        printf '  # %-13s %-10s %s\n' "${field}:" "${default:-\"\"}" "${why}"
    fi
}

# gentian_sync_deployments_checkout [check] — bring the checkout up to origin.
#
# Step 0 reads the checkout to decide whether this cluster has a definition,
# and commits into it. A checkout behind its remote answers both wrongly: a
# definition deleted on origin still "exists" here, so the interview is
# skipped, and the push that follows is refused with "fetch first". Observed
# on the first fresh install after a cluster was removed through GitHub.
#
# Fast-forwards when that is all it takes. When it is not -- local commits
# origin does not have, or uncommitted changes -- it refuses and says exactly
# what to run, because either choice (keep or discard) is the operator's.
# With `check`, it only reports: --validate and --dry-run promise to change
# nothing, and moving the checkout is a change.
gentian_sync_deployments_checkout() {
    resolve_deployments_path
    local mode="${1:-sync}" path="${GENTIAN_DEPLOYMENTS_PATH}" branch behind ahead dirty
    [[ -d "${path}/.git" ]] || return 0   # scaffold_cluster_deployment reports this
    git -C "${path}" remote get-url origin >/dev/null 2>&1 || return 0
    branch="$(git -C "${path}" branch --show-current 2>/dev/null || true)"
    branch="${branch:-${GENTIAN_DEPLOYMENTS_BRANCH:-main}}"
    if ! git -C "${path}" fetch -q origin "${branch}" 2>/dev/null; then
        warn "Could not fetch origin/${branch} of the deployments repository; using the checkout as it is."
        return 0
    fi
    # An empty remote -- a repository created a minute ago -- has no branch to
    # compare against, and nothing to be behind.
    git -C "${path}" rev-parse --verify -q "origin/${branch}" >/dev/null 2>&1 || return 0
    behind="$(git -C "${path}" rev-list --count "HEAD..origin/${branch}" 2>/dev/null || echo 0)"
    ahead="$(git -C "${path}" rev-list --count "origin/${branch}..HEAD" 2>/dev/null || echo 0)"
    (( behind > 0 )) || return 0
    dirty="$(git -C "${path}" status --porcelain 2>/dev/null || true)"
    if (( ahead == 0 )) && [[ -z "${dirty}" ]]; then
        if [[ "${mode}" == "check" ]]; then
            warn "The deployments checkout is ${behind} commit(s) behind origin/${branch}; an install fast-forwards it."
            return 0
        fi
        git -C "${path}" merge -q --ff-only "origin/${branch}" >/dev/null 2>&1 || {
            error "Could not fast-forward ${path} to origin/${branch}."
            return 1
        }
        info "Deployments checkout fast-forwarded to origin/${branch} (${behind} commit(s))."
        return 0
    fi
    error "The deployments checkout is ${behind} commit(s) behind origin/${branch} and cannot be fast-forwarded:"
    if (( ahead > 0 )); then
        error "  it has ${ahead} local commit(s) origin does not:"
        git -C "${path}" log --oneline "origin/${branch}..HEAD" 2>/dev/null | while IFS= read -r line; do
            error "    ${line}"
        done
    fi
    [[ -n "${dirty}" ]] && error "  it has uncommitted changes."
    error "  Step 0 would read a stale definition and its push would be refused. Reconcile first:"
    error "    git -C ${path} pull --rebase origin ${branch}     # keep the local work"
    error "    git -C ${path} reset --hard origin/${branch}      # or discard it"
    return 1
}

scaffold_cluster_deployment() {
    # The files are only useful inside the checkout they get committed from.
    # Writing them into a bare directory produces a tree nothing tracks, which
    # looks like success and installs nothing.
    if [[ ! -d "${GENTIAN_DEPLOYMENTS_PATH}/.git" ]]; then
        error "${GENTIAN_DEPLOYMENTS_PATH} is not a git checkout of gentian-deployments."
        error "  Clone it there first:"
        error "    git clone ${GENTIAN_DEPLOYMENTS_REPO:-<deployments-repo>} ${GENTIAN_DEPLOYMENTS_PATH}"
        error "  Or point GENTIAN_DEPLOYMENTS_PATH at an existing checkout."
        return 1
    fi

    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel"
    local stage="${GENTIAN_DEPLOYMENTS_STAGE:-dev}"
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
    local domain="${KERNEL_DOMAIN:?KERNEL_DOMAIN must be resolved before scaffold_cluster_deployment}"
    local generated=0

    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/${stage}.yaml" ]]; then
        warn "gentian-deployments/profiles/${stage}.yaml does not exist yet."
        warn "  Stage-tier policy (logLevel, ACME issuer, etc.) has no home for '${stage}' —"
        warn "  add it (see profiles/dev.yaml for the existing example) before continuing."
    fi
    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/_base.yaml" ]]; then
        warn "gentian-deployments/profiles/_base.yaml does not exist yet."
        warn "  Cross-stage shared policy (platformSecurityPolicy, etc.) has no home —"
        warn "  add it before continuing (see profiles/_base.yaml in an existing cluster's repo)."
    fi

    mkdir -p "${kernel_dir}/claims"

    if [[ ! -f "${kernel_dir}/claims/cluster.yaml" ]]; then
        # The claim is named GENTIAN_DEPLOYMENTS_CLUSTER_ID + _STAGE. This file
        # is written only when absent, and gentian_cluster_claim_name() reads
        # the name back from it, so a cluster keeps whatever name it was
        # scaffolded with.
        cat > "${kernel_dir}/claims/cluster.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: ${cluster}-${stage}
  namespace: ${CROSSPLANE_NAMESPACE:-crossplane-system}
spec:
  kernelDomain: ${domain}
  # Who administers this cluster, by the Keycloak group they are in. The
  # director reads this file from git -- not the object in the cluster -- so
  # the assignment is written here rather than left to the schema's default,
  # which a file reader never sees. Without it the director grants nobody a
  # cluster role and every console the person is entitled to disappears.
  platformRoles:
    admin: ${PLATFORM_ADMIN_GROUP:-gentian:platform:admin}
$(_claim_cluster_fields)
EOF
        info "Scaffolded ${kernel_dir}/claims/cluster.yaml"
        generated=1
    fi

    if [[ ! -f "${kernel_dir}/claims/suze.yaml" ]]; then
        cat > "${kernel_dir}/claims/suze.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Suze
metadata:
  name: ${cluster}-${stage}-suze
  namespace: ${CROSSPLANE_NAMESPACE:-crossplane-system}
spec:
  environment: ${stage}
  idpNamespace: kernel-authentication
  fgaNamespace: kernel-authorization
  compositeDeletePolicy: Background
  openfga:
    chartVersion: "0.3.10"
EOF
        info "Scaffolded ${kernel_dir}/claims/suze.yaml"
        generated=1
    fi

    # The deployments repository's own credential.
    #
    # Without this claim nothing composes the Secret the operator and the
    # director mount to PUSH. Both can clone a public repository without it,
    # so everything looks installed until the first write: a tenant created
    # in the console commits locally and then fails with "could not read
    # Username for https://github.com", which names neither this claim nor
    # the token it wants.
    #
    # The claim is scaffolded whether or not a token has been supplied. It
    # emits a CredentialRequirement, so `make check-credentials` can say the
    # push credential is missing -- which is the whole point of declaring a
    # requirement rather than discovering it.
    if [[ ! -f "${kernel_dir}/claims/deployments-repository.yaml" ]]; then
        cat > "${kernel_dir}/claims/deployments-repository.yaml" <<EOF
# The repository this cluster is described by, and the credential that writes
# to it. The name matters: the composition emits \`<name>-git-credentials\`,
# which is what kernel/values.yaml names as appLifecycle.deployments.
#
# Supply the token with GENTIAN_DEPLOYMENTS_GIT_TOKEN in install.env and run
# B-08; it is stored at the vault path below and materialised from there.
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: deployments
  namespace: ${CROSSPLANE_NAMESPACE:-crossplane-system}
spec:
  type: git
  role: deployments
  # Writable: this is the one repository the platform commits to. Every
  # change the console makes lands here as a commit by the person who asked.
  writable: true
  branch: ${GENTIAN_DEPLOYMENTS_BRANCH:-main}
  endpoints:
    inCluster: ${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments}
  credential:
    vaultPath: gentian-os/kernel/repositories/deployments
    displayName: "Deployments repository write access"
    phase: bootstrap
    authType: ${GENTIAN_DEPLOYMENTS_AUTH_TYPE:-basic}
    validate:
      type: git-https
EOF
        info "Scaffolded ${kernel_dir}/claims/deployments-repository.yaml"
        generated=1
    fi

    # The other three repositories the platform reads: its own charts, the app
    # catalogue and the desktop. Without a claim each one has no AppProject
    # source, so Argo CD refuses to deploy from it, and the catalogue-sync
    # ApplicationSet has nothing to sync -- which reads as an empty catalogue
    # rather than as a missing declaration.
    #
    # Public by default, and that is a real shape rather than a degenerate
    # one: spec.credential is optional precisely so a public repository does
    # not have to name a vault path for a secret that does not exist, which
    # would then sit in the credential manager as a requirement nobody can
    # satisfy. A mirror sets the matching AUTH and gets the credential block.
    local _repo_role _repo_url _repo_branch _repo_auth _repo_file _repo_xrd_role _repo_auth_var
    for _repo_role in gentian-os gentian-apps gentian-ui; do
        _repo_file="${kernel_dir}/claims/${_repo_role}-repository.yaml"
        [[ -f "${_repo_file}" ]] && continue
        # role is the XRD's vocabulary and is not always the repository's
        # name: the catalogue is "apps".
        _repo_xrd_role="${_repo_role}"
        case "${_repo_role}" in
            gentian-os)
                _repo_url="${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}"
                _repo_branch="${GENTIAN_OS_BRANCH:-main}"
                _repo_auth="${GENTIAN_OS_AUTH:-none}"
                _repo_auth_var=GENTIAN_OS_AUTH ;;
            gentian-apps)
                _repo_url="${GENTIAN_APPS_REPO:-https://github.com/gentian-org/gentian-apps}"
                _repo_branch="${GENTIAN_APPS_BRANCH:-main}"
                _repo_auth="${GENTIAN_APPS_AUTH:-none}"
                _repo_auth_var=GENTIAN_APPS_AUTH
                _repo_xrd_role=apps ;;
            gentian-ui)
                _repo_url="${GENTIAN_UI_REPO:-https://github.com/gentian-org/gentian-ui}"
                _repo_branch="${GENTIAN_UI_BRANCH:-main}"
                _repo_auth="${GENTIAN_UI_AUTH:-none}"
                _repo_auth_var=GENTIAN_UI_AUTH ;;
        esac
        cat > "${_repo_file}" <<EOF
# ${_repo_role}, as this cluster reads it. The claim is what gives Argo CD an
# AppProject source for the repository; without one it refuses to deploy from
# it whatever the Application says.
#
# No credential block: this repository is public. Naming a vault path for a
# secret that does not exist puts an unsatisfiable requirement in front of
# whoever runs \`make check-credentials\`. A mirror sets ${_repo_auth_var}
# and re-runs this scaffold to get one.
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: ${_repo_role}
  namespace: ${CROSSPLANE_NAMESPACE:-crossplane-system}
spec:
  type: git
  role: ${_repo_xrd_role}
  writable: false
  branch: ${_repo_branch}
  endpoints:
    inCluster: ${_repo_url}
EOF
        if [[ "${_repo_auth}" != "none" ]]; then
            cat >> "${_repo_file}" <<EOF
  credential:
    vaultPath: gentian-os/kernel/repositories/${_repo_role}
    displayName: "${_repo_role} repository read access"
    phase: bootstrap
    authType: ${_repo_auth}
    validate:
      type: git-https
EOF
        fi
        info "Scaffolded ${_repo_file}"
        generated=1
    done

    if [[ ! -f "${kernel_dir}/values.yaml" ]]; then
        cat > "${kernel_dir}/values.yaml" <<EOF
# Cluster overlay — only what's unique to THIS cluster. Tier-wide policy
# lives in gentian-deployments/profiles/${stage}.yaml (Layer 2); chart
# defaults live in gentian-os/charts/gentian-os/values.yaml (Layer 1).
kernelDomain: ${domain}
stage: ${stage}
llmSupport: ${LLM_SUPPORT:-false}

image:
  tag: "develop"

appLifecycle:
  deployments:
    enabled: true
    cluster: ${cluster}
    repo: ${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments.git}
    # Named from the CLAIM, not the composite and not the repo. The Composition
    # emits <claimName>-git-credentials and B-09 names the claim "deployments",
    # so this is deployments-git-credentials. Scaffolding
    # gentian-deployments-git-credentials pointed the operator at a Secret
    # nothing creates -- and because the volume is optional with a subPath, the
    # kubelet mounted an empty directory there rather than leaving it absent, so
    # installing an app failed with
    #   fatal: unable to open /etc/git/credentials: Is a directory
    gitCredentialsSecret: deployments-git-credentials
EOF
        info "Scaffolded ${kernel_dir}/values.yaml"
        generated=1
    fi

    # Tenant/platform: the platform is a tenant whose realm is the kernel
    # realm (AD-10). Its desktop is the platform-admin console and its members
    # are the platform's administrators, so it exists from the first install,
    # scaffolded here beside the claim rather than deployed later like a
    # customer tenant. The operator adopts the realm it names and refuses to
    # delete the tenant; both follow from isolation.keycloakRealm being the
    # kernel realm, which is the one line below that must not change.
    local platform_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/tenants/platform"
    if [[ ! -f "${platform_dir}/tenant.yaml" ]]; then
        mkdir -p "${platform_dir}"
        cat > "${platform_dir}/tenant.yaml" <<EOF
# The platform tenant. Its realm is the kernel realm: the operator adopts
# that realm rather than creating one, and this tenant cannot be deleted --
# a realm-disable against the kernel realm would lock every administrator
# out at once. Everything else about it is what any tenant gets.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: platform
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  displayName: Platform
  isolation:
    mode: namespace
    keycloakRealm: ${KERNEL_REALM:-kernel}
    databasePrefix: platform_
    s3Prefix: platform-
  deletionPolicy: Retain
  # The base plan's capacity, the same ceiling every deployed tenant starts
  # on (see the tenant-defaults component the deploy command writes).
  quotas:
    requestsCpu: "4"
    requestsMemory: 16Gi
    cpu: "16"
    memory: 32Gi
    storage: 50Gi
    maxApps: 20
  # No apps: the platform desktop's tiles are the kernel consoles the
  # director answers from this account's relations, not catalogue apps.
  apps: []
EOF
        cat > "${platform_dir}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- tenant.yaml
EOF
        info "Scaffolded clusters/${cluster}/tenants/platform"
        generated=1
    fi

    # No cluster-settings.env is written. Everything it carried that describes
    # the cluster is a field on claims/cluster.yaml, emitted above by
    # _claim_cluster_fields and read back by claim_setting before Crossplane
    # exists. Writing both would recreate the second surface this removed.

    if (( generated )); then
        echo ""
        success "Wrote clusters/${cluster}/kernel in ${GENTIAN_DEPLOYMENTS_PATH}."
    else
        info "clusters/${cluster}/kernel is already complete — nothing written."
    fi

    # Committed and pushed here, not left as an instruction.
    #
    # It used to say "commit and push them" and stop. Argo CD syncs claims/
    # from the repository, so a claim left in the working copy is applied by
    # nothing -- and the one that matters most, deployments-repository.yaml,
    # is what gives the director its push credential. Forgetting it produced
    # an install that finished, screens that read, and a first write that
    # answered 503 with nothing pointing back here.
    #
    # The keys this cluster's commits are signed by, published where a
    # reviewer and the cluster both read them from (AD-2).
    gentian_publish_signing_material "${kernel_dir}" || true

    # Nothing is APPLIED: this still contacts no cluster.
    gentian_commit_cluster_deployment "${kernel_dir}" "${cluster}"

    echo ""
    info "clusters/${cluster}/kernel is what this cluster becomes. To change a"
    info "  setting later, edit it there and run ./install.sh: step 0 commits the"
    info "  edit signed and the cluster reconciles."
}

# =============================================================================
# require_cluster_deployment — the forward pass's precondition.
#
# Installing reads this cluster's claims and values from gentian-deployments, so
# an absent file is not something to fill in silently: the generated default
# would decide the cluster's domain, tenancy and exposure model without anyone
# having read it. Name what is missing and stop.
# =============================================================================
# A scaffolded file the cluster never sees.
#
# clusters/<id>/kernel/claims is synced by the gentian-claims ApplicationSet,
# straight from the deployments repository -- so a claim that exists only in
# the working copy is applied by nothing. The install still finishes: every
# step's check() passes, because none of them looks for a claim that Argo was
# supposed to bring. What fails is the first write, long afterwards and with
# nothing connecting it back to here.
#
# So this warns rather than refuses: a working copy mid-review is a legitimate
# state, and an installer that stopped on it would be wrong. Naming what will
# happen is enough, and it is what nobody was told.
# _claims_this_checkout_cannot_apply <kernel-dir> — stale claims, named.
#
# A claim names a kind, and a kind exists here only if crossplane/xrds/ defines
# it. One that names anything else is a file Argo CD will sync and the API
# server will refuse -- and because the refusal happens three layers away, on
# a resource nothing else mentions, it reads as an unrelated Argo error days
# later.
#
# This exists because a leftover claims/infra-data.yaml survived the layout
# that removed the InfraData kind, and step 0 -- which commits whatever is
# dirty under the cluster's kernel directory -- committed and pushed it. The
# file was UNTRACKED, so it had never been reviewed, never been in a diff,
# and the first thing that ever touched it was an installer being helpful.
#
# Names them rather than deleting them. A file somebody put there on purpose
# is not the installer's to remove, and a kind this checkout does not define
# may be one a newer checkout does.
_claims_this_checkout_cannot_apply() {
    local kernel_dir="$1" claim kind
    local -a known=()
    while IFS= read -r kind; do
        [[ -n "${kind}" ]] && known+=("${kind}")
    done < <(grep -h '^    kind:' "${SCRIPT_DIR}"/crossplane/xrds/*.yaml 2>/dev/null |
        awk '{print $2}' | sort -u)
    (( ${#known[@]} > 0 )) || return 0   # no XRDs to compare against: say nothing

    for claim in "${kernel_dir}"/claims/*.yaml; do
        [[ -f "${claim}" ]] || continue
        kind="$(awk '/^kind:/ {print $2; exit}' "${claim}" 2>/dev/null)"
        [[ -n "${kind}" ]] || continue
        local found=0 k
        for k in "${known[@]}"; do [[ "${k}" == "${kind}" ]] && found=1 && break; done
        (( found )) || printf '%s\t%s\n' "$(basename "${claim}")" "${kind}"
    done
}

# gentian_commit_cluster_deployment <kernel-dir> <cluster> — close the loop.
#
# The scaffolder used to write the files and warn that they were uncommitted,
# which left the most important step of bringing a cluster up as something the
# operator had to remember. Argo CD syncs claims/ from the REPOSITORY, so an
# uncommitted deployments-repository.yaml is a director with no push
# credential: the install finishes, every screen reads, and the first write
# answers 503. Nothing in the output said why.
#
# So this commits and pushes, signed with the break-glass key (AD-2). A human
# writing directly to the deployments repository is exactly the break-glass
# case, and giving that act its own key means `git log --show-signature`
# afterwards says which commits a person made and which the director made.
#
# A checkout that is not a repository, or has nothing to commit, is fine and
# returns 0. A commit or a push that FAILS returns 1, and step 0 stops the
# install on it: Argo CD syncs the definition from the repository, so an
# install that went on from here would build a cluster whose own claims never
# reach it -- and the one that matters most, deployments-repository.yaml, is
# the director's push credential. That failure used to be a warning that
# scrolled past, and the install that followed looked fine until its first
# write answered 503.
gentian_commit_cluster_deployment() {
    local kernel_dir="$1" cluster="$2" dirty sign_args branch
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" rev-parse --git-dir >/dev/null 2>&1 || {
        warn "${GENTIAN_DEPLOYMENTS_PATH} is not a git repository, so nothing was committed."
        warn "  Argo CD syncs claims/ from the repository; these files reach the cluster"
        warn "  only from a checkout that has a remote."
        return 0
    }

    dirty="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" status --porcelain -- \
        "clusters/${cluster}/kernel" 2>/dev/null || true)"
    [[ -n "${dirty}" ]] || return 0

    # Refuse to commit a claim nothing here can apply.
    #
    # Committing is the act that makes a file the cluster's problem, so this
    # is the last moment it is still only a file on somebody's disk.
    local stale
    stale="$(_claims_this_checkout_cannot_apply "${kernel_dir}")"
    if [[ -n "${stale}" ]]; then
        error "clusters/${cluster}/kernel has claims this checkout cannot apply:"
        while IFS=$'\t' read -r f k; do
            [[ -n "${f}" ]] && error "    ${f} declares kind ${k}, which no XRD in crossplane/xrds/ defines"
        done <<< "${stale}"
        error ""
        error "  Argo CD would sync them and the API server would refuse them, days"
        error "  from here and with nothing pointing back. Nothing was committed."
        error ""
        error "  Delete the file if its kind is gone, or update this checkout if it"
        error "  is a kind a newer release defines:"
        error "    rm ${kernel_dir}/claims/<file>"
        return 1
    fi

    if ! sign_args="$(gentian_git_sign_args break-glass 2>/dev/null)"; then
        gentian_ensure_signing_key break-glass >/dev/null || {
            _warn_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
            return 0
        }
        sign_args="$(gentian_git_sign_args break-glass)"
    fi

    info "Committing clusters/${cluster}/kernel:"
    while IFS= read -r line; do
        [[ -n "${line}" ]] && info "    ${line}"
    done <<< "${dirty}"

    local -a SIGN
    read -r -a SIGN <<< "${sign_args}"
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" add -- "clusters/${cluster}/kernel" || {
        _warn_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
        return 0
    }
    if ! git -C "${GENTIAN_DEPLOYMENTS_PATH}" \
        -c "user.name=${GENTIAN_COMMITTER_NAME:-Gentian installer}" \
        -c "user.email=${GENTIAN_COMMITTER_EMAIL:-installer@${KERNEL_DOMAIN:-cluster.invalid}}" \
        "${SIGN[@]}" commit -q -m "chore(${cluster}): scaffold the kernel deployment

Written by install.sh (step 0) and signed with this cluster's
break-glass key: before the cluster exists there is no director to write it,
and AD-2 names that case." 2>&1; then
        error "The commit failed; clusters/${cluster}/kernel is still uncommitted."
        _warn_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
        return 1
    fi

    # Explicitly to the branch of the same name, not a bare `git push`. A
    # checkout whose local branch and upstream are named differently -- which
    # a clone of an empty repository produces -- makes a bare push refuse with
    # "the upstream branch of your current branch does not match", and that is
    # a confusing thing to hit while bringing up a cluster.
    branch="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" branch --show-current 2>/dev/null || true)"
    branch="${branch:-${GENTIAN_DEPLOYMENTS_BRANCH:-main}}"
    if ! git -C "${GENTIAN_DEPLOYMENTS_PATH}" push -q origin "HEAD:${branch}" 2>&1; then
        error "Committed, but the push failed. The cluster reads the repository, not"
        error "  this checkout, so nothing committed here reaches it until this succeeds:"
        error "    git -C ${GENTIAN_DEPLOYMENTS_PATH} push origin HEAD:${branch}"
        error "  Then run ./install.sh again."
        return 1
    fi
    success "Committed and pushed clusters/${cluster}/kernel (signed, break-glass)."
}

_warn_uncommitted_cluster_deployment() {
    local kernel_dir="$1" cluster="$2" dirty
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" rev-parse --git-dir >/dev/null 2>&1 || return 0

    # Untracked or modified, under this cluster's kernel directory only: a
    # tenant being edited elsewhere in the repository is not this step's
    # business.
    dirty="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" status --porcelain -- \
        "clusters/${cluster}/kernel" 2>/dev/null)" || return 0
    [[ -n "${dirty}" ]] || return 0

    warn "clusters/${cluster}/kernel has uncommitted changes:"
    while IFS= read -r line; do
        [[ -n "${line}" ]] && warn "    ${line}"
    done <<< "${dirty}"
    warn "  Argo CD syncs claims/ from the repository, not from this checkout,"
    warn "  so anything above reaches the cluster only once it is pushed."
    if grep -q "deployments-repository.yaml" <<< "${dirty}"; then
        warn "  deployments-repository.yaml is among them. Until it is pushed the"
        warn "  director has no push credential: every write answers 503, so no"
        warn "  tenant can be created and no user invited."
    fi
}

# cluster_deployment_missing — the files this cluster's definition still lacks.
#
# Empty output means complete. Split out of require_cluster_deployment so that
# the forward run can ASK rather than refuse: step 0 of an install is now
# "make the definition if it is not there", and that needs the question
# answered before it decides whether to interview anybody.
cluster_deployment_missing() {
    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel"
    local f
    for f in claims/cluster.yaml claims/suze.yaml claims/deployments-repository.yaml values.yaml; do
        [[ -f "${kernel_dir}/${f}" ]] || printf '%s\n' "${f}"
    done
}

require_cluster_deployment() {
    local cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
    local kernel_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/kernel"
    local missing=() f

    # cluster-settings.env is NOT required. The exposure and mail model it used
    # to carry are fields on the claim now, read by claim_setting before the
    # cluster exists, so demanding the file rejected a cluster whose
    # configuration is complete — this one, immediately after migrating it.
    # v5 has no InfraData claim (see scaffold_cluster_deployment).
    #
    # claims/deployments-repository.yaml is required on v5 and was required by
    # nothing before. Its composition emits deployments-git-credentials, which
    # is the Secret the director mounts to push -- so without it the install
    # completes, every screen reads, and the FIRST WRITE answers 503. A tenant
    # cannot be created and a user cannot be invited, which is M2 and M3, and
    # nothing in the install output says why. The claims/ directory is synced
    # from git by the gentian-claims ApplicationSet, so scaffolding the file is
    # only half of it: it has to be committed to reach the cluster.
    while IFS= read -r f; do
        [[ -n "${f}" ]] && missing+=("${f}")
    done < <(cluster_deployment_missing)

    # What the file used to guarantee, checked where it now lives. networkMode
    # decides whether the edge is a LoadBalancer, and static-ip without nodeIp
    # produces a Service whose address nothing pins — the failure the old
    # requirement existed to prevent, now stated against the claim.
    local claim="${kernel_dir}/claims/cluster.yaml"
    if [[ -f "${claim}" ]]; then
        local net
        net="$(yq_get '.spec.networkMode' "${claim}" 2>/dev/null || true)"
        if [[ "${net}" == "static-ip" ]] && ! yq_get '.spec.nodeIp' "${claim}" >/dev/null 2>&1; then
            error "claims/cluster.yaml sets networkMode: static-ip without nodeIp."
            error "  DNS would point at an address the load balancer does not claim."
            return 1
        fi
    fi

    if (( ${#missing[@]} == 0 )); then
        # Not a warning any more. An edit sitting in the working copy at
        # install time is an edit the cluster will not get, and the operator
        # has already said what they want by making it.
        gentian_commit_cluster_deployment "${kernel_dir}" "${cluster}"
        return 0
    fi

    error "clusters/${cluster}/kernel is incomplete in gentian-deployments."
    error "  Missing: ${missing[*]}"
    error "  Path:    ${kernel_dir}"
    error ""
    error "  An install writes them first, asking for each setting:"
    error "    ./install.sh"
    error ""
    error "  If this cluster's configuration lives elsewhere, check"
    error "  GENTIAN_DEPLOYMENTS_CLUSTER_ID and GENTIAN_DEPLOYMENTS_PATH."
    return 1
}
