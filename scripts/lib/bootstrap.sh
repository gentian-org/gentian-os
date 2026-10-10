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

    if ! VAULT_ADDR=$(gentian_service_addr openbao "${OPENBAO_NAMESPACE:-$(ns_kernel secrets)}" 8200 https); then
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
# `declare -F _derive` — which _keycloak_smtp_settings tested before deriving the
# Postfix password, as it now tests _derived below — was therefore false
# everywhere else. On every
# MAIL_SERVICE_MODE=system cluster that test failed, so Keycloak realm SMTP was
# skipped with "SMTP credentials incomplete" and the realm could not send an
# invitation or a password reset, while the credentials it needed existed.
# =============================================================================
_derive() {
    if [[ "${SECRET_MODE:-derived}" == "random" ]]; then
        openssl rand -hex 32
    else
        _derived "${1}" "${2}"
    fi
}

# _derived <context> <purpose> — the derivation itself, whatever the mode.
#
# _derive answers a different value on every call under secretMode random, so
# it only suits a caller that offers the value to a path created once and
# keeps what the path already held. A caller that hands the value straight to
# its users, and is called more than once, needs the same answer each time:
# the kernel realm's own mail login is asked for twice in one run, and under
# random the realm was configured with one password while the Secret the mail
# server learns it from held another. That one is derived in both modes.
_derived() {
    echo -n "${1}:${2}" | openssl dgst -sha256 \
        -hmac "${MASTER_PASSWORD}${DERIVATION_SALT}" | awk '{print $2}'
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
            --arg d "$(_derive postgres registrar_user)" \
            '{postgres_password:$a,keycloak_user_password:$b,keycloak_extensions_user_password:$c,openfga_user_password:$h,portal_shell_user_password:$p,registrar_user_password:$d}')" \
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
        xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n "${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}" \
            -o jsonpath='{.spec.resourceRef.name}' 2>/dev/null || true)
        if (( SECONDS > deadline )); then
            error "Claim ${claim_name} was never bound to a composite after 60s."
            error "  kubectl describe cluster.gentianos.io ${claim_name} -n ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}"
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
# manage: internal/master-password, dns/cloudflare,
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
    seed_licence_report_key
}

# =============================================================================
# seed_licence_report_key — the key this cluster signs its licence reports with
# =============================================================================
# 32 random bytes, as hex, under their own path. The operator derives an
# Ed25519 key pair from them (internal/licencereport): the private half signs
# each report and the public half travels in it, so the address the reports go
# to can tell one cluster's reports from anybody else's without having been
# given anything first.
#
# Random and not derived from the master password, because it must not be
# reproducible from anything else the cluster holds. Written once and never
# again: a new seed is a new identity, and every report after it would look
# like another cluster's. So an existing value is left alone, including by a
# run that cannot read it.
#
# Not written at all where the report is turned off: a cluster that sends
# nothing is given no key to send it with. Turning the report on later means
# running this step again while the installer can still write to the vault.
#
# The path sits under gentian-os/kernel/ for the reason the repository
# credentials do: it is the one prefix ESO may read.
# =============================================================================
seed_licence_report_key() {
    local path="gentian-os/kernel/licence-report" have
    if [[ "$(gentian_licence_report_enabled)" != "true" ]]; then
        info "Licence report is off: no signing key is seeded."
        return 0
    fi
    have="$(bao kv get -mount=secret -field=signing_seed "${path}" 2>/dev/null || true)"
    if [[ -n "${have}" ]]; then
        info "Licence report signing key already present; left as it is."
        return 0
    fi
    # Absent, or unreadable. `kv put` with check-and-set 0 writes only when
    # the path has never been written, so a path this run merely failed to
    # read is not replaced.
    if ! bao kv put -mount=secret -cas=0 "${path}" \
        "signing_seed=$(openssl rand -hex 32)" >/dev/null 2>&1; then
        warn "The licence report signing key could not be written to OpenBao."
        warn "  No report is sent without it; the operator records that it has no key."
        return 0
    fi
    success "Licence report signing key stored."
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
    local row req name user_var token_var
    for row in "${GENTIAN_REPO_CREDENTIALS[@]}"; do
        IFS='|' read -r req name _ _ <<<"${row}"
        user_var="$(_repo_credential "${req}" username)"
        token_var="$(_repo_credential "${req}" token)"

        # The platform's own repositories are public unless install.env says
        # they authenticate, and then they have no path to seed. Their
        # Repository claims gate on the same paths as the deployments one, so
        # where they do authenticate they need the value here just as much.
        if [[ "${req}" != "deployments-repository" && "$(_repo_credential_mode "${req}")" == "none" ]]; then
            continue
        fi
        if [[ -z "${!token_var:-}" ]]; then
            if [[ "${req}" == "deployments-repository" ]]; then
                info "No deployments repository token supplied; skipping its OpenBao path."
            fi
            continue
        fi

        info "Seeding gentian-os/kernel/repositories/${name}..."
        # Written every time, not once: a rotated token has to replace the
        # one that is there.
        if ! bao kv put -mount=secret "gentian-os/kernel/repositories/${name}" \
            "username=${!user_var:-x-access-token}" \
            "password=${!token_var}" >/dev/null 2>&1; then
            error "Could not write the ${name} repository credential to OpenBao."
            error "  Its Repository claim will not become satisfied without it."
            return 1
        fi
        success "Repository credential stored: ${name}."
    done
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

    # Before C-01 there is no openbao ClusterSecretStore, so no ExternalSecret
    # can produce the Secrets the issuers read: waiting here only spent three
    # and a half minutes of every install before the same warning. C-01 runs
    # this function again once the store is Ready.
    if [[ "$(kubectl get clustersecretstore openbao \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" != "True" ]]; then
        if [[ -n "$(_not_ready_cluster_issuers)" ]]; then
            info "ClusterIssuers wait for the secret store, which C-01 composes; re-synced there."
        fi
        return 0
    fi

    # The Secrets the not-ready issuers name, waited for before the nudge.
    # The re-sync above stops at the first quiet poll, which can come before
    # External Secrets has written the Secret -- the store having only just
    # become Ready. A nudge then re-latches the issuer on the same absence,
    # and cert-manager does not watch a solver's Secret, so nothing retries:
    # the DNS-01 issuer stayed failed after a finished install.
    local cm_ns secret deadline
    cm_ns="$(gentian_cert_manager_namespace 2>/dev/null || echo "${CERT_MANAGER_NAMESPACE:-$(ns_kernel edge)}")"
    deadline=$(( SECONDS + 90 ))
    while read -r secret; do
        [[ -n "${secret}" ]] || continue
        until kubectl get secret "${secret}" -n "${cm_ns}" >/dev/null 2>&1; do
            (( SECONDS < deadline )) || break
            sleep 3
        done
    done <<< "$(kubectl get clusterissuers.cert-manager.io -o json 2>/dev/null | jq -r '
        .items[]
        | select([(.status.conditions // [])[] | select(.type == "Ready" and .status == "True")] | length == 0)
        | [.spec.acme.solvers[]? | .. | objects | to_entries[] | select(.key | test("SecretRef$")) | .value.name // empty] | .[]' 2>/dev/null | sort -u || true)"

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
    local xr_name xr_ready mr_count infra_pg_ready infra_mdb_ready infra_redis_ready infra_minio_ready argocd_url

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
                -n "${GENTIAN_SYSTEM_NAMESPACE:-$(ns_kernel control)}" \
                -o jsonpath='{.data.bootstrapCredentialRevoked}' 2>/dev/null)" == "true" ]]; then
        _gentian_handover_done=1
    fi

    local claim_name
    claim_name="$(gentian_cluster_claim_name)"
    xr_name=$(kubectl get cluster.gentianos.io "${claim_name}" -n "${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}" \
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
    suze_xr=$(kubectl get suze.gentianos.io "$(gentian_suze_claim_name)" -n "${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}" \
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
    # Who is in charge of what, and where. On a single-tenancy cluster there
    # are two administrators and two addresses, and this is the screen that
    # has to say which is which; on a multi-tenancy one, tenants come next.
    print_roles_summary
    echo ""
    echo -e "${GREEN}  Inspect authz stack:${NC}"
    echo -e "${GREEN}    kubectl get xsuze,suze -n ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}${NC}"
    echo ""
    echo -e "${GREEN}  Inspect Crossplane managed resources:${NC}"
    echo -e "${GREEN}    kubectl get managed -l crossplane.io/composite=${xr_name}${NC}"
    echo -e "${GREEN}    kubectl get release.helm.crossplane.io | grep ${xr_name}${NC}"
    echo ""
    echo -e "${GREEN}  ArgoCD:${NC}"
    echo -e "${GREEN}    URL  : ${argocd_url}${NC}"
    # No password: Argo CD's local admin is off (D-03), and it is signed in
    # to through the kernel realm like every other kernel UI.
    echo -e "${GREEN}    Sign in with Keycloak, as a member of gentian:platform:admin${NC}"
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
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-$(ns_kernel control)}"
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
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-$(ns_kernel control)}"
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
        echo -e "${YELLOW}      3. sign in as the platform admin at https://platform.${KERNEL_DOMAIN:-<kernel-domain>}/${NC}"
        echo -e "${YELLOW}      4. ./install.sh --only E-03      (revoke and finish)${NC}"
    elif [[ "${proven}" != "true" ]]; then
        echo -e "${YELLOW}      1. move the recovery kit somewhere safe${NC}"
        echo -e "${YELLOW}      2. sign in as the platform admin at https://platform.${KERNEL_DOMAIN:-<kernel-domain>}/${NC}"
        echo -e "${YELLOW}      3. ./install.sh --only E-03      (revoke and finish)${NC}"
    else
        echo -e "${YELLOW}    Someone has signed in and a kit exists, so only the revocation${NC}"
        echo -e "${YELLOW}    is left:${NC}"
        echo -e "${YELLOW}      ./install.sh --only E-03${NC}"
    fi
    echo ""
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
        # Asked by prompt_cluster_settings; production unless answered
        # otherwise, on every stage. Staging used to be the dev default, for
        # its rate limits, but the kernel's own sign-in fetches Keycloak's
        # discovery document through the in-cluster gateway, which serves
        # this certificate: Envoy Gateway refuses a staging chain, so a dev
        # install stopped at D-03 with every SecurityPolicy Invalid. The rate
        # limits are met instead by keeping the issued wildcard across purges
        # (save_kernel_wildcard).
        local ae="${ACME_ENV:-production}"
        printf '    # production. staging is not needed for rebuilds: the installer\n'
        printf '    # keeps the issued wildcard across purges, so a reinstall orders\n'
        printf '    # no new certificate. It also breaks the kernel sign-in, which does\n'
        printf '    # not trust its chain. The option may be removed.\n'
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
        printf '      namespace: %s\n' "${CA_BUNDLE_SECRET_NAMESPACE:-$(ns_kernel edge)}"
    else
        printf '    # caBundleSecretRef:         only read when issuerMode is private-ca\n'
    fi

    printf '\n'
    printf '  # Where mail goes.\n'
    printf '  #   external  relay through an SMTP provider; supply the smtp-relay\n'
    printf '  #             credential to the custodian after install\n'
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
    _claim_default_line tenancyMode  "${TENANCY_MODE:-}"  multi   'any number of user tenants, each on its own subdomain; single = exactly one user tenant, named user, on the cluster domain itself'
    _claim_default_line secretMode   "${SECRET_MODE:-}"   derived 'every kernel secret reproducible from the master password; random = independent'
    _claim_default_line routingMode  "${ROUTING_MODE:-}"  gateway 'Envoy Gateway plus the Gateway API; the only supported value'
    _claim_default_line storageClass "${STORAGE_CLASS:-}" ''      'empty means the clusters default StorageClass'

    printf '\n'
    printf '  # Where the backup private key lives. On by default: it goes to OpenBao as\n'
    printf '  # well as the recovery kit, so a platform admin can restore without\n'
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
        printf '    # The console of the gateway (models, keys, spend) at llm.<kernelDomain>,\n'
        printf '    # for platform administrators. Off by default: nothing the platform\n'
        printf '    # does needs it, and off means no route and no way in from the edge.\n'
        if [[ "${LLM_CONSOLE:-false}" == "true" ]]; then
            printf '    console:\n'
            printf '      enabled: true\n'
        else
            printf '    # console:\n'
            printf '    #   enabled: true\n'
        fi
        if [[ -n "${GPU_TIME_SLICE_REPLICAS:-}" ]]; then
            printf '    gpuTimeSliceReplicas: %s\n' "${GPU_TIME_SLICE_REPLICAS}"
        else
            printf '    # gpuTimeSliceReplicas: 1   workloads sharing one physical GPU\n'
        fi
        printf '    # The models this cluster serves on its own GPUs. The gateway offers\n'
        printf '    # one model per entry; the platform does not start the vLLM instance\n'
        printf '    # behind it yet, so run one before listing it here.\n'
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
        printf '    # adds its models to the gateway; removing one removes them.\n'
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
        printf '  #   console:\n'
        printf '  #     enabled: false          set true to serve the console of the gateway at llm.<kernelDomain>\n'
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
    # The App Store and its catalogue are set, because a vanilla installation
    # comes with them: the store is where people are sent for apps, and the
    # Gentian source is where the Operations Console's entries come from. Changing a source is an edit to this file
    # rather than an environment variable on the director -- opening a
    # catalogue to a tenant is then a commit with an author and a date, and
    # "what may this cluster install from" is answerable without cluster
    # access. A cluster with no source still works: it materialises nothing on
    # reference, and its profiles arrive with the kernel.
    _claim_catalogue_section
    # The API extensions of add-ons. Recorded here even while nothing reads
    # it, so the choice an installer made is in git beside everything else it
    # chose; the grant and the service entries follow when the catalogue
    # serves them.
    if [[ "${GENTIAN_DISABLE_API_EXTENSIONS:-0}" == "1" ]]; then
        printf '  # apiExtensions: disabled (--disable-api-extensions): no Operations Console\n'
    else
        printf '  # apiExtensions: enabled: the Operations Console installs by default\n'
    fi
    return 0
}

# ensure_platform_concierge_exposure <platform tenant directory>
#
# The one surface an installation publishes by itself: the concierge, on the
# cluster's bare domain. It is the page anybody typing the cluster's address
# meets before they have a session, so it cannot be behind one, and a cluster
# nobody can find the sign-in of is not installed.
#
# It is published the way every perimeter surface is, and in the same place:
# exposures.yaml beside the tenant, which is the registry the director reads
# and writes. Written anywhere else it would not show in that registry, and
# the next surface published through the director would replace the list it
# was in. So this writes the file the director would have written, with an
# owner and a review date a year out, and lists it as a patch.
#
# The owner is the installer because nobody has an account yet. Once the file
# exists it is the director's and is left alone, an empty list included:
# withdrawing the concierge is a decision this must not undo. Succeeds only
# when it wrote.
ensure_platform_concierge_exposure() {
    local dir="$1"
    local file="${dir}/exposures.yaml" manifest="${dir}/tenant.yaml" kustomization="${dir}/kustomization.yaml"
    [[ -f "${manifest}" ]] || return 1

    # An earlier installer appended the entry to the tenant's manifest itself.
    # The block is this function's own, from its comment to the end of the
    # file, and is taken out so the surface is declared once.
    local wrote=1
    if grep -q '^  # What this tenant publishes with no session in front of it' "${manifest}"; then
        local kept
        kept="$(sed '/^  # What this tenant publishes with no session in front of it/,$d' "${manifest}")"
        printf '%s\n' "${kept}" > "${manifest}"
        info "tenants/platform/tenant.yaml: the concierge's publication moves to exposures.yaml."
        wrote=0
    fi

    if [[ ! -f "${file}" ]]; then
        local now review
        now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        # A year from now. GNU date takes -d, BSD date takes -v; one of the
        # two is what this host has.
        review="$(date -u -d '+365 days' +%Y-%m-%dT00:00:00Z 2>/dev/null || date -u -v+365d +%Y-%m-%dT00:00:00Z)"
        cat > "${file}" <<EXPOSURES
# Managed by the director: what this tenant publishes to the internet.
#
# The concierge is the page on the bare domain of the cluster, which sends a
# person to the sign-in of their workspace. The installer published it; from
# here on this file is the director's, and withdrawing or reviewing the entry
# is done there.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: platform
spec:
  exposures:
    - install: concierge
      exposureName: front
      owner: installer
      reviewAt: ${review}
      reason: The sign-in page on the bare domain of the cluster, published at install.
      publishedAt: ${now}
      lastReviewedBy: installer
      lastReviewedAt: ${now}
EXPOSURES
        info "tenants/platform/exposures.yaml: the concierge is published on the bare domain."
        wrote=0
    fi

    # Listed as a patch, or kustomize never reads it.
    if [[ -f "${file}" && -f "${kustomization}" ]] && ! grep -qx -- '- path: exposures.yaml' "${kustomization}"; then
        if grep -qx 'patches:' "${kustomization}"; then
            local listed
            listed="$(awk '{ print } $0 == "patches:" { print "- path: exposures.yaml" }' "${kustomization}")"
            printf '%s\n' "${listed}" > "${kustomization}"
        else
            [[ -z "$(tail -c 1 "${kustomization}")" ]] || printf '\n' >> "${kustomization}"
            printf 'patches:\n- path: exposures.yaml\n' >> "${kustomization}"
        fi
        wrote=0
    fi
    return "${wrote}"
}

# _remove_retired_apps_repository_claim <kernel dir>
#
# Removes the Repository claim earlier installers scaffolded for the app
# catalogue's git repository, and answers 0 when it removed one.
#
# Only the file this installer wrote, recognised by what it declared: the
# claim named gentian-apps, a git repository with role apps. A file of that
# name saying anything else is somebody's own and is left alone.
_remove_retired_apps_repository_claim() {
    local file="$1/claims/gentian-apps-repository.yaml"
    [[ -f "${file}" ]] || return 1
    grep -q '^  name: gentian-apps$' "${file}" || return 1
    grep -q '^  type: git$' "${file}" || return 1
    grep -q '^  role: apps$' "${file}" || return 1
    rm -f "${file}"
    info "Removed ${file}: profiles are no longer copied from the gentian-apps git repository;"
    info "  they arrive one at a time from the catalogue source on the Cluster claim."
    return 0
}

# gentian_catalogue_url
#
# The address a new Cluster claim names for the default catalogue, `gentian`.
#
# gentian-apps publishes two: the released catalogue, built from its main, and
# the development one, built from its develop. Which a cluster reads follows
# from which gentian-os it is installed from, the ref in GENTIAN_OS_BRANCH (or,
# unset, this checkout's branch -- gentian_os_ref): a release tag or
# main is released software and reads the released catalogue; any other branch
# is software under development and reads the catalogue under development,
# whose profiles may need what only that platform has. A ref that cannot be
# read gets the released one. GENTIAN_CATALOGUE_URL, set, is the answer
# whatever the ref.
#
# Asked when a claim is written and at no other time: an existing claim's
# source is somebody's decision and is not rewritten.
gentian_catalogue_url() {
    if [[ -n "${GENTIAN_CATALOGUE_URL:-}" ]]; then
        printf '%s\n' "${GENTIAN_CATALOGUE_URL}"
        return 0
    fi
    case "$(gentian_os_ref)" in
        v[0-9]*.[0-9]*.[0-9]* | main | "")
            printf '%s\n' "https://gentian-org.github.io/gentian-apps" ;;
        *)
            printf '%s\n' "https://gentian-org.github.io/gentian-apps/develop" ;;
    esac
}

# gentian_catalogue_reason
#
# One line saying why gentian_catalogue_url answers as it does: the comment
# above the address in the claim, and what step 0 prints.
gentian_catalogue_reason() {
    local ref
    if [[ -n "${GENTIAN_CATALOGUE_URL:-}" ]]; then
        printf '%s\n' "Set by GENTIAN_CATALOGUE_URL when this claim was written."
        return 0
    fi
    ref="$(gentian_os_ref)"
    case "${ref}" in
        v[0-9]*.[0-9]*.[0-9]* | main)
            printf '%s\n' "The released catalogue: this cluster was installed from gentian-os ${ref}." ;;
        "")
            printf '%s\n' "The released catalogue: the gentian-os ref installed from could not be read." ;;
        *)
            printf '%s\n' "The development catalogue: this cluster was installed from the gentian-os branch ${ref}, not from a release." ;;
    esac
}

# _claim_catalogue_section
#
# The claim's catalogue section: the App Store people are sent to, and the
# source its entries are fetched from. One function, because it is written in
# two places that must agree: a new claim's scaffold, and an existing claim
# that has none (ensure_claim_catalogue_section).
_claim_catalogue_section() {
    printf '\n'
    printf '  # Catalogues this cluster may fetch profiles from. A tenant installing\n'
    printf '  # "gentian/nextcloud-base-ce" gets the bundle from the source named\n'
    printf '  # gentian, at the digest the install asks for -- the source itself is\n'
    printf '  # not trusted.\n'
    printf '  #\n'
    printf '  # A source named here is offered to every tenant: the cluster lists its\n'
    printf '  # entries to them as installable from here, without the App Store.\n'
    printf '  # This is not a licence: whether an app arrives is decided by whether\n'
    printf '  # the tenant holds a credential for the repository it is pulled from.\n'
    printf '  catalogue:\n'
    printf '    # The base address of the App Store API: where the App Store app on\n'
    printf '    # this cluster reads what is on offer and what a tenant has acquired.\n'
    if [[ "$(gentian_licence_report_enabled)" == "true" ]]; then
        printf '    storeUrl: %s\n' "${GENTIAN_STORE_URL:-https://store-service.aluvian.io}"
    else
        printf '    #\n'
        printf '    # Not named here: the App Store needs licence reporting, which is\n'
        printf '    # turned off on this cluster (--no-licence-report).\n'
        printf '    # storeUrl: %s\n' "${GENTIAN_STORE_URL:-https://store-service.aluvian.io}"
    fi
    printf '    sources:\n'
    printf '      - name: gentian\n'
    # The public catalogue of the gentian-apps repository, which is where the
    # ce and pe profiles are published: the released one or the development
    # one, by what is being installed (gentian_catalogue_url).
    printf '        # %s\n' "$(gentian_catalogue_reason)"
    printf '        url: %s\n' "$(gentian_catalogue_url)"
    printf '      # - name: in-house\n'
    printf '      #   url: https://git.example.com/profiles\n'
}

# ensure_claim_catalogue_section <claim file>
#
# An installation loads the App Store by default, and that has to hold for a
# claim written before the default existed as well as for a new one: such a
# claim carries the section as a comment, the director reads no store from it,
# and the desktop then has no App Store tile for anybody.
#
# So a claim with no spec.catalogue is given the default one. A claim that
# says anything at all there is left alone, and that is how a cluster goes
# without a store: `catalogue: {}` is a decision somebody wrote down, where an
# absent key is only a key nobody wrote. The one edit made to an existing
# section is removing the fields the schema no longer has
# (_claim_drop_catalogue_access, _claim_drop_catalogue_tenants).
ensure_claim_catalogue_section() {
    local claim="$1"
    [[ -f "${claim}" ]] || return 0
    if yq_get '.spec.catalogue' "${claim}" >/dev/null 2>&1; then
        _claim_drop_catalogue_access "${claim}"
        _claim_drop_catalogue_tenants "${claim}"
        _claim_warn_retired_catalogue_addresses "${claim}"
        # A store somebody named stays named: the claim is theirs. The desktop
        # is told the store is unavailable all the same, by the usher.
        if [[ "$(gentian_licence_report_enabled)" != "true" ]] \
            && yq_get '.spec.catalogue.storeUrl' "${claim}" >/dev/null 2>&1; then
            warn "claims/cluster.yaml names an App Store, and licence reporting is off:"
            warn "  the App Store is not offered on this cluster. Remove spec.catalogue.storeUrl"
            warn "  to say so in the claim as well."
        fi
        return 0
    fi
    # Appended to the file, which is only inside spec while spec is the last
    # top-level key. A claim somebody has reordered is theirs to edit.
    local last
    last="$(grep -E '^[A-Za-z]' "${claim}" | tail -n 1)"
    if [[ "${last}" != "spec:" ]]; then
        warn "claims/cluster.yaml names no catalogue, and spec is not its last section,"
        warn "  so the App Store default is not added. Add spec.catalogue by hand, or"
        warn "  write 'catalogue: {}' there to say this cluster has no store."
        return 0
    fi
    # A file that does not end in a newline would have the section glued to
    # its last line.
    [[ -z "$(tail -c 1 "${claim}")" ]] || printf '\n' >> "${claim}"
    _claim_catalogue_section >> "${claim}"
    info "claims/cluster.yaml named no catalogue: the App Store default was added."
    info "  Catalogue: $(gentian_catalogue_url)"
    info "  $(gentian_catalogue_reason)"
    info "  To run without a store, set 'catalogue: {}' under spec."
}

# preview_claim_catalogue_section <claim file> — what
# ensure_claim_catalogue_section would do to the claim, without doing it.
#
# For --dry-run and --validate. The edit is made to a copy outside the
# checkout, so the claim is judged by exactly the code an install runs and is
# not touched by it; whatever that code says is printed under a line saying
# none of it happened.
preview_claim_catalogue_section() {
    local claim="$1" copy said
    [[ -f "${claim}" ]] || return 0
    copy="$(mktemp)"
    cat "${claim}" > "${copy}"
    said="$(ensure_claim_catalogue_section "${copy}" 2>&1 || true)"
    said="${said//${copy}/${claim}}"
    if ! cmp -s "${claim}" "${copy}"; then
        gentian_would "edit claims/cluster.yaml, then commit and push the edit signed"
        info "  What an install would report having done to it:"
        printf '%s\n' "${said}" | sed 's/^/    /'
    elif [[ -n "${said}" ]]; then
        # Nothing to edit: only what it has to say about the claim as it is.
        printf '%s\n' "${said}"
    fi
    rm -f "${copy}"
}

# _claim_warn_retired_catalogue_addresses <claim file>
#
# Two addresses an earlier installer wrote into every new claim never served
# anything: the store at gentian.org/apps and the catalogue at
# store.gentian.org/catalogue. A claim still naming them gives its tenants an
# App Store that cannot reach a store and a catalogue that lists nothing, and
# nothing on the cluster says why. The claim is not rewritten -- it is
# somebody's -- so the installer says which line it is and what a new claim
# would say there.
_claim_warn_retired_catalogue_addresses() {
    local claim="$1" store urls
    store="$(yq_get '.spec.catalogue.storeUrl' "${claim}" 2>/dev/null || true)"
    if [[ "${store%/}" == "https://gentian.org/apps" ]]; then
        warn "claims/cluster.yaml names the App Store ${store} (spec.catalogue.storeUrl),"
        warn "  an address earlier installs wrote that never served a store: the App Store"
        warn "  app would list nothing. A new claim names ${GENTIAN_STORE_URL:-https://store-service.aluvian.io}."
        warn "  Edit the line in ${claim} and run ./install.sh again."
    fi
    urls="$(yq_get '.spec.catalogue.sources[].url' "${claim}" 2>/dev/null || true)"
    if grep -qE '^https://store\.gentian\.org/catalogue/?$' <<< "${urls}"; then
        warn "claims/cluster.yaml names the catalogue https://store.gentian.org/catalogue"
        warn "  (spec.catalogue.sources), an address earlier installs wrote that never served"
        warn "  one: no app could be listed or installed from it. A new claim names"
        warn "  $(gentian_catalogue_url)."
        warn "  Edit the line in ${claim} and run ./install.sh again."
    fi
    return 0
}

# _claim_drop_catalogue_access <claim file>
#
# A catalogue source used to say `access: entitled` or `access: open`. The
# schema no longer has the field, and a claim still carrying it is refused
# when it is applied, because an unknown field is an error rather than something ignored.
# So the lines are removed from a claim written before, and only there: under
# spec.catalogue, at a source's own indent.
_claim_drop_catalogue_access() {
    local claim="$1" tmp
    grep -qE '^      +access: *(entitled|open) *$' "${claim}" || return 0
    tmp="$(mktemp)"
    awk '
        /^  catalogue:/              { inside = 1; print; next }
        inside && /^  [A-Za-z]/      { inside = 0 }
        inside && /^      +access: *(entitled|open) *$/ { next }
        { print }
    ' "${claim}" > "${tmp}" && cat "${tmp}" > "${claim}"
    rm -f "${tmp}"
    info "claims/cluster.yaml: removed 'access' from its catalogue sources; the schema no longer has the field."
}

# _claim_drop_catalogue_tenants <claim file>
#
# A catalogue source used to list the tenants it was open to. The list gated
# nothing -- an install asks whether the person may install apps in the
# tenant, and never asked about the source -- so the schema no longer has it:
# a source the claim names is offered to every tenant. A claim still carrying
# `tenants:` under a source is refused when it is applied, for the same reason
# as `access` above, so it is removed from a claim written before.
#
# The value may be written on the key's line (`tenants: [demo]`), or as a
# list below it, and the key may be the first of its source (`- tenants:`).
# In that last case the dash moves to the source's next key, which would
# otherwise belong to the source before it.
_claim_drop_catalogue_tenants() {
    local claim="$1" tmp
    grep -qE '^    +(- +)?tenants:' "${claim}" || return 0
    tmp="$(mktemp)"
    awk '
        function indent(line) { match(line, /^ */); return RLENGTH }
        /^  catalogue:/         { inside = 1; skipping = 0; dash = ""; print; next }
        inside && /^  [A-Za-z]/ { inside = 0; skipping = 0; dash = "" }
        inside && skipping {
            # The value of the key being dropped: anything indented deeper
            # than the key, and list items written at the key own indent.
            if ($0 !~ /^ *$/ && (indent($0) > key || (indent($0) == key && $0 ~ /^ *- /))) next
            skipping = 0
        }
        inside && /^      +tenants:/ { key = indent($0); skipping = 1; next }
        inside && /^    +- +tenants:/ {
            match($0, /^ *- +/); key = RLENGTH; dash = substr($0, 1, RLENGTH)
            skipping = 1; next
        }
        inside && dash != "" {
            if ($0 !~ /^ *$/ && indent($0) == key) $0 = dash substr($0, key + 1)
            dash = ""
        }
        { print }
    ' "${claim}" > "${tmp}"
    if ! cmp -s "${tmp}" "${claim}"; then
        cat "${tmp}" > "${claim}"
        info "claims/cluster.yaml: removed 'tenants' from its catalogue sources; a source is offered to every tenant."
    fi
    rm -f "${tmp}"
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
#
# With `check`, it only reports: --validate and --dry-run promise to change
# nothing, and that includes the checkout's own .git. So origin is ASKED where
# its branch is (ls-remote) rather than fetched from: a fetch stores origin's
# objects in the checkout and moves its origin/<branch>, which is a write even
# though no file anybody edits changes. The price is that a checkout which is
# behind cannot be told by how many commits, only that it is.
gentian_sync_deployments_checkout() {
    resolve_deployments_path
    local mode="${1:-sync}" path="${GENTIAN_DEPLOYMENTS_PATH}" branch behind ahead dirty
    [[ -d "${path}/.git" ]] || return 0   # scaffold_cluster_deployment reports this
    git -C "${path}" remote get-url origin >/dev/null 2>&1 || return 0
    branch="$(git -C "${path}" branch --show-current 2>/dev/null || true)"
    branch="${branch:-${GENTIAN_DEPLOYMENTS_BRANCH:-main}}"

    if [[ "${mode}" == "check" ]]; then
        _deployments_checkout_report "${path}" "${branch}"
        return
    fi

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
        git -C "${path}" merge -q --ff-only "origin/${branch}" >/dev/null 2>&1 || {
            error "Could not fast-forward ${path} to origin/${branch}."
            return 1
        }
        info "Deployments checkout fast-forwarded to origin/${branch} (${behind} commit(s))."
        return 0
    fi
    _deployments_checkout_refuse "${path}" "${branch}" "${behind} commit(s) behind" "${ahead}" "${dirty}"
}

# _deployments_checkout_report <path> <branch> — the same verdict, read-only.
#
# Returns what the sync would: 0 where an install would go on (up to date, or
# behind and able to fast-forward), 1 where it would refuse.
_deployments_checkout_report() {
    local path="$1" branch="$2" theirs ours ahead=0 dirty
    if ! theirs="$(git -C "${path}" ls-remote origin "refs/heads/${branch}" 2>/dev/null)"; then
        warn "Could not reach origin of the deployments repository; using the checkout as it is."
        return 0
    fi
    theirs="${theirs%%[[:space:]]*}"
    # An empty remote, or one without the branch: nothing to be behind.
    [[ -n "${theirs}" ]] || return 0
    ours="$(git -C "${path}" rev-parse -q --verify HEAD 2>/dev/null || true)"
    [[ "${theirs}" != "${ours}" ]] || return 0
    # origin's head is a commit this checkout already has in its own history:
    # the checkout is ahead of origin, not behind it.
    if git -C "${path}" merge-base --is-ancestor "${theirs}" HEAD 2>/dev/null; then
        return 0
    fi
    # Local commits origin does not have, by the last origin/<branch> this
    # checkout fetched. --no-optional-locks: without it `git status` rewrites
    # the index to refresh it, which is a write to the checkout.
    if git -C "${path}" rev-parse --verify -q "origin/${branch}" >/dev/null 2>&1; then
        ahead="$(git -C "${path}" rev-list --count "origin/${branch}..HEAD" 2>/dev/null || echo 0)"
    fi
    dirty="$(git -C "${path}" --no-optional-locks status --porcelain 2>/dev/null || true)"
    if (( ahead == 0 )) && [[ -z "${dirty}" ]]; then
        warn "The deployments checkout is behind origin/${branch} (origin is at ${theirs:0:12}); an install fast-forwards it."
        warn "  This run reads the checkout as it is, so what it reports is the definition before that."
        return 0
    fi
    _deployments_checkout_refuse "${path}" "${branch}" "behind" "${ahead}" "${dirty}"
}

# _deployments_checkout_refuse <path> <branch> <how far behind> <ahead> <dirty>
_deployments_checkout_refuse() {
    local path="$1" branch="$2" behind="$3" ahead="$4" dirty="$5"
    error "The deployments checkout is ${behind} origin/${branch} and cannot be fast-forwarded:"
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
    # Step 0 writes the cluster's definition, publishes its keys and pushes.
    # prepare_run does not call it under --dry-run or --validate; this is for
    # the caller that one day forgets.
    if gentian_read_only; then
        gentian_would "write whatever clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-<cluster>} still lacks, and commit and push it signed"
        return 0
    fi
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

    # Before anything is written: the gentian-os Repository claim names the
    # ref this cluster follows, and a detached checkout with no
    # GENTIAN_OS_BRANCH has none to name. This wrote "main" there.
    if [[ ! -f "${kernel_dir}/claims/gentian-os-repository.yaml" && -z "$(gentian_os_ref)" ]]; then
        error "GENTIAN_OS_BRANCH is not set and this checkout has no branch to read,"
        error "  so the gentian-os Repository claim cannot name the ref this cluster follows."
        error "  Set GENTIAN_OS_BRANCH in install.env (a branch, or a release tag such as v0.4.0)."
        return 1
    fi

    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/${stage}.yaml" ]]; then
        warn "profiles/${stage}.yaml does not exist in the deployments repository."
        warn "  Argo CD reads it as a values file of the platform's own chart and cannot"
        warn "  render the chart without it: the install would stop at D-01, with no"
        warn "  operator. Add the file (it may hold a comment and nothing else), commit"
        warn "  and push it before continuing."
    fi
    if [[ ! -f "${GENTIAN_DEPLOYMENTS_PATH}/profiles/_base.yaml" ]]; then
        warn "profiles/_base.yaml does not exist in the deployments repository."
        warn "  Argo CD reads it as a values file of the platform's own chart and cannot"
        warn "  render the chart without it: the install would stop at D-01, with no"
        warn "  operator. Add the file (it may hold a comment and nothing else), commit"
        warn "  and push it before continuing."
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
  namespace: ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}
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
        info "  Catalogue: $(gentian_catalogue_url)"
        info "  $(gentian_catalogue_reason)"
        generated=1
    fi

    if [[ ! -f "${kernel_dir}/claims/suze.yaml" ]]; then
        cat > "${kernel_dir}/claims/suze.yaml" <<EOF
apiVersion: gentianos.io/v1alpha1
kind: Suze
metadata:
  name: ${cluster}-${stage}-suze
  namespace: ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}
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
  namespace: ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}
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

    # The other two repositories the platform reads: its own charts and the
    # desktop. Without a claim each one has no AppProject source, so Argo CD
    # refuses to deploy from it.
    #
    # Public by default, and that is a real shape rather than a degenerate
    # one: spec.credential is optional precisely so a public repository does
    # not have to name a vault path for a secret that does not exist, which
    # would then sit in the custodian as a requirement nobody can
    # satisfy. A mirror sets the matching AUTH and gets the credential block.
    #
    # The app catalogue is not one of them. Its git repository was claimed
    # here so that an ApplicationSet could copy every profile in it into the
    # cluster; a profile now arrives one at a time through the director, from
    # the catalogue source the Cluster claim names (_claim_catalogue_section),
    # and nothing on the cluster reads that git repository. A deployments
    # checkout scaffolded before then still holds the claim file, and it is
    # removed here so that a fresh install from it declares no such claim.
    # Claims are synced without pruning, so on a cluster that already applied
    # it the Repository stays until it is deleted there; it composes no
    # ApplicationSet any more either way (docs/install-reference.md).
    _remove_retired_apps_repository_claim "${kernel_dir}" && generated=1
    local _repo_role _repo_url _repo_branch _repo_auth _repo_file _repo_auth_var
    for _repo_role in gentian-os gentian-ui; do
        _repo_file="${kernel_dir}/claims/${_repo_role}-repository.yaml"
        [[ -f "${_repo_file}" ]] && continue
        case "${_repo_role}" in
            gentian-os)
                _repo_url="${GENTIAN_OS_REPO:-https://github.com/gentian-org/gentian-os}"
                _repo_branch="$(gentian_os_ref)"
                _repo_auth="${GENTIAN_OS_AUTH:-none}"
                _repo_auth_var=GENTIAN_OS_AUTH ;;
            gentian-ui)
                _repo_url="${GENTIAN_UI_REPO:-https://github.com/gentian-org/gentian-ui}"
                _repo_branch="$(gentian_ui_branch)"
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
  namespace: ${CROSSPLANE_NAMESPACE:-$(ns_kernel provisioning)}
spec:
  type: git
  role: ${_repo_role}
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
    cluster: ${cluster}
    repo: ${GENTIAN_DEPLOYMENTS_REPO:-https://github.com/gentian-org/gentian-deployments.git}
    # Named from the CLAIM, not the composite and not the repo. The Composition
    # emits <claimName>-git-credentials and B-09 names the claim "deployments",
    # so this is deployments-git-credentials: the Secret the director clones
    # and pushes with.
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
  # The cluster administrator (admin@<kernel>) has no password until its
  # holder sets one through a single-use link the handover issues. Whether
  # activating it also enrols a second factor:
  admin:
    requireMFA: ${CLUSTER_ADMIN_REQUIRE_MFA:-true}
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
    # The concierge's publication, for the tenant just written and for one
    # written before the concierge was a published component alike.
    if ensure_platform_concierge_exposure "${platform_dir}"; then
        generated=1
    fi

    # The user tenant of a single-tenancy cluster: beside the platform
    # tenant, committed with it, admitted after the handover.
    if scaffold_user_tenant "${cluster}"; then
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

    # The profiles every tenant gets without asking (AD-14, the installer's
    # default profile): materialised into the same directory the director
    # materialises an installed entry into, so the operator sees them like
    # any other.
    #
    # Not `|| true` any more: a catalogue that cannot be reached returns 0 and
    # the install goes on, and what returns 1 -- a profile that is not the
    # build its digest names -- is a reason to stop before anything of this
    # run is committed.
    _scaffold_default_profiles "${cluster}" || return 1

    # Nothing is APPLIED: this still contacts no cluster.
    gentian_commit_cluster_deployment "${kernel_dir}" "${cluster}"
    gentian_sign_unsigned_head "${kernel_dir}" "${cluster}" || true

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
# _cluster_scaffold_paths <cluster> — what step 0 scaffolds, and so commits:
# the kernel directory and Tenant/platform beside it. Committing only kernel/
# left the platform tenant on the install host's disk, the tenants
# ApplicationSet generated nothing, and D-03 waited for a Tenant that could
# never arrive.
_cluster_scaffold_paths() {
    printf '%s\n' "clusters/$1/kernel" "clusters/$1/tenants/platform" "clusters/$1/catalogue"
    # And the user tenant of a single-tenancy cluster, when its manifest is
    # there. Only then: a path that does not exist fails `git add` for all of
    # them.
    if [[ -n "$(gentian_user_tenant)" ]]; then
        printf '%s\n' "clusters/$1/tenants/${USER_TENANT_NAME}"
    fi
}

# =============================================================================
# The user tenant of a single-tenancy cluster.
#
# A cluster has one of two tenancy modes (docs/design/multi-tenancy.md §3).
# multi: the platform tenant plus any number of user tenants, each created
# through the director after the install. single: the platform tenant plus
# exactly one user tenant, always named "user", which lives on the cluster's
# own addresses -- desktop.<domain>, admin.<domain>, <app>.<domain>.
#
# Only under single does the install create a tenant, and it is that one.
# Step 0 writes its manifest beside the platform tenant's and commits both,
# signed, like the rest of the cluster's definition. Nothing admits it early:
# the cluster holds every tenant but the platform's back until the platform
# admin has signed in once (the handover gate, internal/webhook), and this
# manifest carries no override. So it sits in git, refused, through the whole
# install; the handover (E-03) ends as on any cluster; and E-04 then asks
# Argo CD to try the manifest again, waits for the tenant, and hands its
# administrator account over. No credential is needed for that which the
# handover takes away: E-04 uses the kubeconfig and nothing else.
# =============================================================================

# The name is fixed: the operator, its admission webhook and the director all
# refuse any other user tenant under tenancyMode single (internal/tenancy).
USER_TENANT_NAME="user"

# gentian_tenancy_mode — this cluster's tenancy mode, single or multi.
#
# The claim's, once there is a claim: it is what the cluster is built from and
# what the operator reads, and a claim that does not name the mode means the
# default, multi -- whatever this run's environment says. A TENANCY_MODE in
# install.env that disagrees with an existing claim would otherwise have the
# install scaffold and wait for a user tenant on a cluster that is not a
# single-tenancy one. Before there is a claim, the mode is what the run was
# told (step 0's answer, or TENANCY_MODE), which is what the claim is then
# written from.
gentian_tenancy_mode() {
    local claim="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/kernel/claims/cluster.yaml"
    local mode=""
    if [[ -f "${claim}" ]]; then
        mode="$(yq_get '.spec.tenancyMode' "${claim}" 2>/dev/null || true)"
    else
        mode="${TENANCY_MODE:-}"
    fi
    if [[ "${mode}" != "single" ]]; then
        mode="multi"
    fi
    printf '%s\n' "${mode}"
}

# gentian_user_tenant — "user" when this is a single-tenancy cluster whose
# definition holds the user tenant's manifest, and nothing otherwise.
#
# Nothing under multi, where a tenant of that name is an ordinary one the
# install has no part in. Nothing when the manifest is absent, either: there
# is then no tenant for the install to wait for or hand over, and saying
# otherwise would have a step wait fifteen minutes for a tenant nobody
# declared.
gentian_user_tenant() {
    local file="${GENTIAN_DEPLOYMENTS_PATH:-}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}/tenants/${USER_TENANT_NAME}/tenant.yaml"
    if [[ "$(gentian_tenancy_mode)" == "single" && -f "${file}" ]]; then
        printf '%s\n' "${USER_TENANT_NAME}"
    fi
    return 0
}

# scaffold_user_tenant <cluster> — write the user tenant's manifest on a
# single-tenancy cluster. Returns 0 when it wrote something.
#
# The manifest is the one the director writes for a new tenant (tenantManifest
# in internal/director/gitops/tenants.go) with one switch on, and nothing
# more: no annotation admits it ahead of the handover. The switch is
# spec.perimeter.adminsApprove, which lets the user admin approve what the
# tenant puts on the internet. Every other tenant starts with it off; here
# the cluster has one tenant for users. Under multi nothing is written. Under
# single, three cases write nothing as well, and each says why:
#
#   - The tenant has a manifest already. Never rewritten: a second run finds
#     the first one's work.
#   - The cluster's definition holds another tenant for users. A
#     single-tenancy cluster carries exactly one, and the operator would
#     refuse them; which of them stays is a person's decision, not a
#     scaffold's.
#   - The tenant was in this cluster's definition before and was removed. A
#     retired tenant keeps its data unless it was purged, and an install run
#     must not quietly attach a new tenant to it.
scaffold_user_tenant() {
    local cluster="$1" name="${USER_TENANT_NAME}"
    if [[ "$(gentian_tenancy_mode)" != "single" ]]; then
        return 1
    fi
    local tenants_dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/tenants"
    local dir="${tenants_dir}/${name}" rel="clusters/${cluster}/tenants/${name}/tenant.yaml"
    local f other others=""

    if [[ -f "${dir}/tenant.yaml" ]]; then
        return 1
    fi
    for f in "${tenants_dir}"/*/tenant.yaml; do
        [[ -f "${f}" ]] || continue
        other="$(basename "$(dirname "${f}")")"
        [[ "${other}" == "platform" ]] && continue
        others="${others:+${others}, }${other}"
    done
    if [[ -n "${others}" ]]; then
        warn "tenancyMode is single and the user tenant was not written: this cluster's"
        warn "  definition already holds ${others}. A single-tenancy cluster carries the"
        warn "  platform tenant and exactly one user tenant, named ${name}, and refuses"
        warn "  every other. Retire ${others} first, or set tenancyMode: multi."
        return 1
    fi
    if [[ -n "$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" log -1 --format=%H -- "${rel}" 2>/dev/null || true)" ]]; then
        warn "The user tenant was not written: clusters/${cluster}/tenants/${name}"
        warn "  was part of this cluster's definition before and was removed. A retired"
        warn "  tenant keeps its data unless it was purged, so the install does not bring"
        warn "  it back by itself. As the platform admin, once the cluster is up:"
        warn "    kubectl gentian tenants create ${name}"
        return 1
    fi

    mkdir -p "${dir}"
    cat > "${dir}/tenant.yaml" <<EOF
# Tenant ${name}: the user tenant of this single-tenancy cluster, written by
# the install.
#
# The cluster's users live here, in a realm of their own, on the cluster's
# own addresses: the desktop at desktop.<domain>, the administration console
# at admin.<domain>, each app at <app>.<domain>. The platform tenant beside
# it holds the platform admin and takes no apps; its desktop is at
# platform.<domain>.
#
# The cluster admits no tenant but the platform's until the platform admin
# has signed in once, and nothing here asks for an exception: this file waits
# in git, and the install brings the tenant up after the handover.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: ${name}
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  displayName: "User"
  # The user admin's account has no password until its holder sets one,
  # through a single-use link the install issues after the handover
  # (afterwards: kubectl gentian tenants activate-admin ${name}).
  admin:
    requireMFA: true
  isolation:
    # A namespace and a realm of its own. The realm is what keeps this
    # tenant's people apart from the platform admin's.
    mode: namespace
    keycloakRealm: ${name}
    databasePrefix: ${name}_
    s3Prefix: ${name}-
  # Retain, so retiring the tenant does not take its data with it. Changing
  # this to Delete is a deliberate, reviewable edit.
  deletionPolicy: Retain
  # The base plan's capacity, which is what every tenant starts on.
  quotas:
    requestsCpu: "4"
    requestsMemory: 16Gi
    cpu: "16"
    memory: 32Gi
    storage: 50Gi
    maxApps: 20
  # Whether the user admin may approve and withdraw this tenant's public
  # addresses. The install turns it on, because this cluster has one tenant
  # for users; the switch is the platform admin's:
  #   kubectl gentian tenants set ${name} --admins-approve-public-addresses=true|false
  perimeter:
    adminsApprove: true
  # Apps are installed through the director, which appends to this list.
  apps: []
EOF
    cat > "${dir}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- tenant.yaml
EOF
    info "Scaffolded clusters/${cluster}/tenants/${name} (the user tenant; admitted after the handover)"
    return 0
}

# print_roles_summary — who is in charge of what on this cluster, and where
# each signs in. The closing words of an install, in the terms the docs use:
# platform admin; user admin (single-tenancy); tenant admin (multi-tenancy).
print_roles_summary() {
    local domain="${KERNEL_DOMAIN:-<kernel-domain>}"
    echo -e "${GREEN}  Who is in charge of what:${NC}"
    echo -e "${GREEN}    platform admin — in charge of the platform:${NC}"
    echo -e "${GREEN}      https://platform.${domain}/     admin@${domain}${NC}"
    echo -e "${GREEN}      A new activation link: ./install.sh --activate-admin${NC}"
    if [[ "$(gentian_tenancy_mode)" == "single" ]]; then
        echo -e "${GREEN}    user admin — in charge of the users and the user tenant:${NC}"
        echo -e "${GREEN}      https://desktop.${domain}/      user-admin@${domain}${NC}"
        echo -e "${GREEN}      A new activation link: kubectl gentian tenants activate-admin ${USER_TENANT_NAME}${NC}"
        echo -e "${GREEN}    This is a single-tenancy cluster: the user tenant is its one tenant for${NC}"
        echo -e "${GREEN}    users, and https://${domain}/ leads to its desktop.${NC}"
        if [[ -z "$(gentian_user_tenant)" ]]; then
            echo -e "${YELLOW}    The user tenant is not in this cluster's definition. As the platform admin:${NC}"
            echo -e "${YELLOW}      kubectl gentian login && kubectl gentian tenants create ${USER_TENANT_NAME}${NC}"
        fi
    else
        echo -e "${GREEN}    tenant admin — in charge of one tenant and its users; one per tenant.${NC}"
        echo -e "${GREEN}    This is a multi-tenancy cluster, and the install creates no tenant for users.${NC}"
        echo -e "${GREEN}    The platform admin creates them in the admin console (Tenants), or with the CLI:${NC}"
        if ! command -v kubectl-gentian >/dev/null 2>&1; then
            echo -e "${GREEN}      make -C ${SCRIPT_DIR} install-plugin${NC}"
        fi
        echo -e "${GREEN}      kubectl gentian login${NC}"
        echo -e "${GREEN}      kubectl gentian tenants create <name>${NC}"
        echo -e "${GREEN}      kubectl gentian tenants activate-admin <name> [--recovery-email <address>]${NC}"
        echo -e "${GREEN}    A tenant's desktop is https://desktop.<name>.${domain}/, and https://${domain}/${NC}"
        echo -e "${GREEN}    asks for an e-mail address and sends each person to theirs.${NC}"
    fi
}

# _scaffold_default_profiles <cluster> -- the store's entries a vanilla
# installation comes with, written as materialised profiles.
#
# GENTIAN_DEFAULT_PROFILES lists them, comma separated: each the https address
# of a profile in a catalogue (<catalogue>/profiles/<name>.yaml), optionally
# pinned with @sha256:<digest>. The default is the Operations Console from the
# store's catalogue. --disable-api-extensions writes none.
#
# Each is held to a digest before a byte of it is written -- the pin, or else
# what the catalogue's index lists -- and written as the director writes an
# install of the same bundle, with its origin and its bundle recorded
# (scripts/lib/catalogue.sh; AD-14).
#
# A catalogue that cannot be reached is a warning and not a failed install:
# the OS needs none of these, and the Admin Console promotes what is missing.
# Everything else that keeps a profile from being placed stops step 0 -- bytes
# that do not hash to the digest above all -- because the alternative is a
# cluster that came up without saying which build it was given.
_scaffold_default_profiles() {
    local cluster="$1" dir list entry
    local -a entries=()
    dir="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${cluster}/catalogue"
    # The directory and its kustomization exist whatever goes in them: the
    # gentian-catalogue Application syncs this path from the first install,
    # and a path that does not exist is a sync error, not an empty catalogue.
    mkdir -p "${dir}"
    if [[ ! -f "${dir}/kustomization.yaml" ]]; then
        printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n' > "${dir}/kustomization.yaml"
    fi
    if [[ "${GENTIAN_DISABLE_API_EXTENSIONS:-0}" == "1" ]]; then
        info "Default profiles skipped (--disable-api-extensions)."
        return 0
    fi
    list="$(_default_profiles_configured)"
    [[ -n "${list}" ]] || return 0
    IFS=',' read -r -a entries <<< "${list}"
    for entry in ${entries[@]+"${entries[@]}"}; do
        entry="$(printf '%s' "${entry}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [[ -n "${entry}" ]] || continue
        _default_profile_place "${cluster}" "${dir}" "${entry}" || return 1
    done
    return 0
}

gentian_commit_cluster_deployment() {
    local kernel_dir="$1" cluster="$2" dirty sign_args branch
    local -a paths
    local _p
    # Whoever calls it: a run that changes nothing does not commit.
    if gentian_read_only; then
        report_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
        return 0
    fi
    paths=()
    while IFS= read -r _p; do paths+=("${_p}"); done < <(_cluster_scaffold_paths "${cluster}")
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" rev-parse --git-dir >/dev/null 2>&1 || {
        warn "${GENTIAN_DEPLOYMENTS_PATH} is not a git repository, so nothing was committed."
        warn "  Argo CD syncs claims/ from the repository; these files reach the cluster"
        warn "  only from a checkout that has a remote."
        return 0
    }

    dirty="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" status --porcelain -- \
        "${paths[@]}" 2>/dev/null || true)"
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

    info "Committing clusters/${cluster} (kernel/, tenants/platform/):"
    while IFS= read -r line; do
        [[ -n "${line}" ]] && info "    ${line}"
    done <<< "${dirty}"

    local -a SIGN
    read -r -a SIGN <<< "${sign_args}"
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" add -- "${paths[@]}" || {
        _warn_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
        return 0
    }
    if ! git -C "${GENTIAN_DEPLOYMENTS_PATH}" \
        -c "user.name=${GENTIAN_COMMITTER_NAME:-Gentian installer}" \
        -c "user.email=${GENTIAN_COMMITTER_EMAIL:-installer@${KERNEL_DOMAIN:-cluster.invalid}}" \
        "${SIGN[@]}" commit -q -m "chore(${cluster}): scaffold the kernel deployment

Written by install.sh (step 0) and signed with this cluster's
break-glass key: before the cluster exists there is no director to write it,
and AD-2 names that case.${_GENTIAN_DEFAULT_PROFILE_NOTES:+

${_GENTIAN_DEFAULT_PROFILE_NOTES}}" 2>&1; then
        error "The commit failed; clusters/${cluster} is still uncommitted."
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
    success "Committed and pushed clusters/${cluster} (signed, break-glass)."
}

# gentian_sign_unsigned_head -- a break-glass commit on top of a head Argo CD
# would refuse.
#
# Argo CD verifies the newest commit of the deployments repository (AD-2,
# gpg mode head), so one unsigned commit there stops the cluster syncing all
# of it. A director that had no key yet made exactly that, and a purge does
# not touch the repository: the next install then came up blind, with every
# tenant ApplicationSet refusing the repository. When the head is not signed
# by a key this cluster trusts, an empty commit signed with the break-glass
# key puts a trusted one on top -- the case AD-2 names, recorded as such.
gentian_sign_unsigned_head() {
    local kernel_dir="$1" cluster="$2" ids head_key head_sig branch sign_args
    # It fetches, merges, commits and pushes: none of it in a read-only run.
    gentian_read_only && return 0
    [[ -f "${kernel_dir}/signing/keys.env" ]] || return 0
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" fetch -q origin 2>/dev/null || return 0
    branch="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" branch --show-current 2>/dev/null || true)"
    branch="${branch:-${GENTIAN_DEPLOYMENTS_BRANCH:-main}}"
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" merge -q --ff-only "origin/${branch}" 2>/dev/null || true
    ids="$(gentian_signing_keys_from_deployment "${kernel_dir}" 2>/dev/null || true)"
    sign_args="$(gentian_git_sign_args break-glass 2>/dev/null)" || return 0
    local -a SIGN
    read -r -a SIGN <<< "${sign_args}"
    head_sig="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" "${SIGN[@]}" log -1 --format='%G?' "origin/${branch}" 2>/dev/null || true)"
    head_key="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" "${SIGN[@]}" log -1 --format='%GK' "origin/${branch}" 2>/dev/null || true)"
    if [[ "${head_sig}" == "G" || "${head_sig}" == "U" ]] && [[ -n "${head_key}" ]] && grep -qi "${head_key}" <<< "${ids}"; then
        return 0
    fi
    warn "The newest commit of the deployments repository is not signed by a key"
    warn "  this cluster trusts, so Argo CD would refuse the whole repository."
    warn "  Adding an empty commit signed with the break-glass key on top."
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" \
        -c "user.name=${GENTIAN_COMMITTER_NAME:-Gentian installer}" \
        -c "user.email=${GENTIAN_COMMITTER_EMAIL:-installer@${KERNEL_DOMAIN:-cluster.invalid}}" \
        "${SIGN[@]}" commit -q --allow-empty -m "chore(${cluster}): sign the head of the deployments repository

The newest commit was not signed by a key this cluster trusts, so Argo CD
refused the repository (AD-2). This empty commit, signed with the
break-glass key, puts a trusted head on top; nothing else changes." 2>&1 || {
        error "The break-glass commit failed; Argo CD will keep refusing the repository."
        return 1
    }
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" push -q origin "HEAD:${branch}" 2>&1 || {
        error "Committed, but the push failed:  git -C ${GENTIAN_DEPLOYMENTS_PATH} push origin HEAD:${branch}"
        return 1
    }
    success "Signed the head of the deployments repository (break-glass)."
}

# report_uncommitted_cluster_deployment <kernel-dir> <cluster> — what an
# install would commit and push from the checkout, for a run that does not.
#
# The same paths and the same question gentian_commit_cluster_deployment
# asks, with --no-optional-locks: without it `git status` rewrites the index
# to refresh it, and the checkout is one of the things this run leaves alone.
report_uncommitted_cluster_deployment() {
    local kernel_dir="$1" cluster="$2" dirty line _p
    local -a paths
    paths=()
    while IFS= read -r _p; do paths+=("${_p}"); done < <(_cluster_scaffold_paths "${cluster}")
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" rev-parse --git-dir >/dev/null 2>&1 || return 0
    dirty="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" --no-optional-locks status --porcelain -- \
        "${paths[@]}" 2>/dev/null || true)"
    if [[ -z "${dirty}" ]]; then
        info "clusters/${cluster} has no uncommitted change; an install would commit nothing from it."
        return 0
    fi
    gentian_would "commit these changes in clusters/${cluster}, signed with the break-glass key, and push them"
    while IFS= read -r line; do
        [[ -n "${line}" ]] && info "    ${line}"
    done <<< "${dirty}"
    return 0
}

_warn_uncommitted_cluster_deployment() {
    local kernel_dir="$1" cluster="$2" dirty
    command -v git >/dev/null 2>&1 || return 0
    git -C "${GENTIAN_DEPLOYMENTS_PATH}" rev-parse --git-dir >/dev/null 2>&1 || return 0

    # Untracked or modified, under what step 0 scaffolds only: any other
    # tenant being edited in the repository is not this step's business.
    local -a paths
    local _p
    paths=()
    while IFS= read -r _p; do paths+=("${_p}"); done < <(_cluster_scaffold_paths "${cluster}")
    dirty="$(git -C "${GENTIAN_DEPLOYMENTS_PATH}" status --porcelain -- \
        "${paths[@]}" 2>/dev/null)" || return 0
    [[ -n "${dirty}" ]] || return 0

    warn "clusters/${cluster} has uncommitted changes:"
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
    if gentian_read_only; then
        preview_claim_catalogue_section "${claim}"
        preview_default_profiles "${cluster}"
    else
        ensure_claim_catalogue_section "${claim}"
    fi
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
        #
        # Under --dry-run and --validate it is reported and left where it is.
        # This call is what committed and pushed a cluster's definition from a
        # dry run: the guard in prepare_run covered the step that WRITES the
        # definition and not this one, which was thought of as a check.
        if gentian_read_only; then
            report_uncommitted_cluster_deployment "${kernel_dir}" "${cluster}"
            return 0
        fi
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
