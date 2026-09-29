#!/usr/bin/env bash
# step: B-10-deployment-signing
# phase: secrets
# requires: B-08-seed-secrets
# provides: both deployment signing keys in Argo CD's keyring, and the director's private key in the vault
# mutates: ConfigMap argocd-gpg-keys-cm in the gitops namespace; the vault path gentian-os/kernel/signing/director

# AD-2's enforcement half: Argo CD syncs the deployments repository only for
# commits signed by the director or by the break-glass key.
#
# Two things have to be true for that, and neither is declarative:
#
#   - Argo CD's repo-server verifies against a keyring built from the
#     ConfigMap argocd-gpg-keys-cm, which Argo CD's own install manifest owns.
#     A chart of ours cannot render into it without fighting whoever applied
#     it last, so the keys are patched in.
#   - The director signs with a private key it has to be given. It goes into
#     the vault here; External Secrets projects it, and the director's chart
#     mounts it.
#
# Both public halves come from the DEPLOYMENTS REPOSITORY rather than from
# this host's keyring, because the repository is what the cluster is supposed
# to agree with. An install host whose keyring has drifted -- a second
# operator, a rebuilt laptop -- should fail here loudly rather than quietly
# teach the cluster to trust a key the repository never mentioned.
#
# WHY PHASE B, between the bootstrap chart and the appsets.
#
# B-01 renders the AppProject, and it renders the sourceIntegrity policy when
# the key ids are in the deployments checkout -- which they are, because
# --prepare-deployment put them there. C-02 creates the ApplicationSet that
# syncs claims/ from that repository, and that is the first thing Argo CD
# verifies. Between those two the keyring has to be filled, or the very first
# sync of the cluster's own claims is refused for want of a key. Anywhere
# after C-02 is too late; anywhere before A-06 has no Argo CD to patch.

_signing_kernel_dir() {
    printf '%s/clusters/%s/kernel' \
        "${GENTIAN_DEPLOYMENTS_PATH}" "${GENTIAN_DEPLOYMENTS_CLUSTER_ID}"
}

_signing_vault_path() {
    printf 'secret/data/gentian-os/kernel/signing/director'
}

check() {
    local ns dir ids id
    ns="$(_argocd_ns)"
    dir="$(_signing_kernel_dir)"

    # Nothing to enforce if the repository names no keys. A cluster whose
    # deployments checkout predates signing is not broken, it is unarmed, and
    # saying MISSING here would block an install that has no key to install.
    [[ -f "${dir}/signing/keys.env" ]] || return "${CHECK_UNDEFINED}"

    kubectl get configmap argocd-gpg-keys-cm -n "${ns}" >/dev/null 2>&1 || return "${CHECK_MISSING}"
    ids="$(gentian_signing_keys_from_deployment "${dir}")"
    for id in ${ids}; do
        [[ -n "${id}" ]] || continue
        kubectl get configmap argocd-gpg-keys-cm -n "${ns}" \
            -o jsonpath="{.data.${id}}" 2>/dev/null | grep -q "BEGIN PGP PUBLIC KEY" ||
            return "${CHECK_MISSING}"
    done

    # The director's private key. Asked of the vault, not of a Secret: the
    # Secret is External Secrets' to make, and a step that checked the Secret
    # would report satisfied for a vault that had lost the key.
    [[ -n "${BAO_TOKEN:-}" && -n "${BAO_ADDR:-}" ]] || return "${CHECK_UNDEFINED}"
    curl -k -sf -H "X-Vault-Token: ${BAO_TOKEN}" \
        "${BAO_ADDR}/v1/$(_signing_vault_path)" 2>/dev/null |
        grep -q '"private"' || return "${CHECK_MISSING}"
}

apply() {
    banner "Deployment signing"
    local ns dir role id asc args=() body
    ns="$(_argocd_ns)"
    dir="$(_signing_kernel_dir)"

    if [[ ! -f "${dir}/signing/keys.env" ]]; then
        warn "clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/signing is absent."
        warn "  Nothing signs this cluster's deployments yet, and Argo CD verifies"
        warn "  nothing. Run ./install.sh --prepare-deployment to generate the keys."
        return 0
    fi

    # The keyring: one ConfigMap entry per key, named by its long id, holding
    # the armoured public half. Patched rather than applied -- the ConfigMap
    # belongs to Argo CD's install manifest.
    for role in director break-glass; do
        asc="${dir}/signing/${role}.asc"
        [[ -f "${asc}" ]] || { error "Missing ${asc}."; return 1; }
        id="$(awk -F= -v r="${role}" '
            (r=="director"    && $1=="GENTIAN_SIGNING_KEY_DIRECTOR")    {print $2}
            (r=="break-glass" && $1=="GENTIAN_SIGNING_KEY_BREAK_GLASS") {print $2}
        ' "${dir}/signing/keys.env")"
        [[ -n "${id}" ]] || { error "signing/keys.env names no ${role} key."; return 1; }
        args+=(--from-file="${id}=${asc}")
        info "  trusting ${role} ${id}"
    done

    # create --dry-run | apply, so the entries are added to whatever is there
    # rather than replacing a ConfigMap Argo CD may already use.
    kubectl create configmap argocd-gpg-keys-cm -n "${ns}" "${args[@]}" \
        --dry-run=client -o yaml |
        kubectl patch configmap argocd-gpg-keys-cm -n "${ns}" --type merge --patch-file /dev/stdin ||
        {
            error "Could not add the signing keys to argocd-gpg-keys-cm in ${ns}."
            return 1
        }

    # The director's private half, into the vault.
    if [[ -z "${BAO_TOKEN:-}" || -z "${BAO_ADDR:-}" ]]; then
        error "No vault token; the director's signing key cannot be stored."
        return 1
    fi
    local secret_json
    secret_json="$(gentian_export_secret_key director |
        python3 -c 'import json,sys; print(json.dumps({"private": sys.stdin.read()}))')"
    body="$(curl -k -s -o /dev/null -w '%{http_code}' \
        -H "X-Vault-Token: ${BAO_TOKEN}" -H "Content-Type: application/json" \
        -X POST -d "{\"data\": ${secret_json}}" \
        "${BAO_ADDR}/v1/$(_signing_vault_path)")"
    if [[ "${body}" -lt 200 || "${body}" -ge 300 ]]; then
        error "Writing the director's signing key to the vault answered HTTP ${body}."
        return 1
    fi
    success "Argo CD trusts both keys; the director's private key is in the vault."
}

destroy() {
    local ns; ns="$(_argocd_ns)"
    # The ConfigMap belongs to Argo CD; only this cluster's entries go. Left
    # whole, a rebuilt cluster would keep trusting keys nobody holds.
    kubectl delete configmap argocd-gpg-keys-cm -n "${ns}" --ignore-not-found=true >/dev/null 2>&1 || true
}
