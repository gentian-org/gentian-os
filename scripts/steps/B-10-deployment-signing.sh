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
# step 0 put them there. C-02 creates the ApplicationSet that
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
    ns="$(ns_kernel gitops)"
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
    local ns dir role id asc body
    local -a keyfiles=()
    ns="$(ns_kernel gitops)"
    dir="$(_signing_kernel_dir)"

    if [[ ! -f "${dir}/signing/keys.env" ]]; then
        warn "clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/signing is absent."
        warn "  Nothing signs this cluster's deployments yet, and Argo CD verifies"
        warn "  nothing. A full ./install.sh run generates them in its step 0."
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
        keyfiles+=("${id}=${asc}")
        info "  trusting ${role} ${id}"
    done

    # A merge patch of the data map ALONE.
    #
    # `kubectl create configmap --dry-run -o yaml` would be the obvious way to
    # build this, but it emits a whole manifest including
    # `metadata.creationTimestamp: null` -- and in a merge patch a null means
    # "remove this key", so the patch asks the API server to delete a field it
    # manages. Sending only the entries says exactly what is meant: add these
    # keys to whatever Argo CD already has there.
    # A quoted heredoc, not python3 -c: the resolvable lint reads every shell
    # file looking for calls it cannot resolve, and `with open(path) as fh:`
    # at low indent reads as a command named `with`. A heredoc body is data
    # and the lint skips it, which is both true and convenient.
    local patch
    patch="$(python3 - "${keyfiles[@]}" <<'PYEOF'
import json
import sys

data = {}
for arg in sys.argv[1:]:
    key, _, path = arg.partition("=")
    with open(path) as handle:
        data[key] = handle.read()
print(json.dumps({"data": data}))
PYEOF
)" || {
        error "Could not build the keyring patch."
        return 1
    }
    kubectl patch configmap argocd-gpg-keys-cm -n "${ns}" --type merge --patch "${patch}" || {
        error "Could not add the signing keys to argocd-gpg-keys-cm in ${ns}."
        error "  Argo CD creates that ConfigMap; if it is absent, Argo CD is not installed here."
        return 1
    }

    # The director's private half, into the vault.
    if [[ -z "${BAO_TOKEN:-}" || -z "${BAO_ADDR:-}" ]]; then
        error "No vault token; the director's signing key cannot be stored."
        return 1
    fi
    # -c here, not a heredoc: a heredoc IS stdin, so it would replace the key
    # being piped in and json.dumps would encode the script instead.
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
    local ns; ns="$(ns_kernel gitops)"
    # The ConfigMap belongs to Argo CD; only this cluster's entries go. Left
    # whole, a rebuilt cluster would keep trusting keys nobody holds.
    kubectl delete configmap argocd-gpg-keys-cm -n "${ns}" --ignore-not-found=true >/dev/null 2>&1 || true
}
