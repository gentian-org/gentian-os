#!/usr/bin/env bash
# =============================================================================
# scripts/lib/signing.sh — the two keys that may write to gentian-deployments.
# =============================================================================
# Sourced by scripts/lib/load.sh. Do not execute directly.
#
# AD-2: the director is the only process that writes to gentian-deployments,
# commits are signed, and Argo CD syncs only commits signed by the director or
# the break-glass key. This file is the key half of that: making the two keys,
# knowing their ids, and signing with the break-glass one.
#
# Why two, and why the installer holds one
# ----------------------------------------
# There is a moment before the cluster exists when somebody has to write the
# claim that brings it into existence. The director cannot: it is not running,
# and the credential it would push with is composed from the very claim being
# written. So the first commit is a human's, and a human writing directly to
# the deployments repository IS the break-glass case AD-2 names. Giving that
# act its own key, rather than turning signing off for it, means the exception
# is visible in the log afterwards: `git log --show-signature` says which key
# signed each commit, and a cluster's history shows exactly where a person
# reached past the director.
#
# Ed25519, not RSA: the keys are generated on an install host that may have
# very little entropy, and a 4096-bit RSA keygen there can take minutes.
#
# No passphrase on either. A passphrase the installer would have to hold in a
# variable to use non-interactively protects nothing, and one that prompts
# turns every commit into a question. What protects the director's key is
# OpenBao and the Secret's RBAC; what protects the break-glass key is the file
# mode below and the recovery kit it is exported into.
# =============================================================================

# gentian_gpg_home — where the installer keeps its keyring.
#
# Its own directory, not the operator's ~/.gnupg: a platform installing a
# cluster should not add keys to the keyring that person uses for their own
# mail, and a purge should be able to remove what the install created.
gentian_gpg_home() {
    printf '%s' "${GENTIAN_GPG_HOME:-${HOME}/.gentian/gnupg}"
}

# loopback + an empty passphrase, on every call.
#
# The keys carry no passphrase, but gpg still asks its agent for one, and on a
# host with no TTY -- an install run over ssh, or from a pipeline -- the agent
# has no pinentry to ask and the call ends in "agent_genkey failed: Timeout"
# a minute later. Loopback says "I am the pinentry, the answer is empty".
#
# Under --dry-run and --validate the keyring is read and nothing else: no
# directory is made for it, and gpg is told not to bring its trust database up
# to date, which it otherwise does -- by writing it -- on whatever call
# happens to come first after a key changed. A keyring that is not there is
# an empty one, and gpg is not started to find that out: started on a missing
# directory, it creates one.
_gpg() {
    local home; home="$(gentian_gpg_home)"
    if gentian_read_only; then
        [[ -d "${home}" ]] || return 1
        gpg --homedir "${home}" --batch --yes --quiet --no-auto-check-trustdb \
            --pinentry-mode loopback --passphrase '' "$@"
        return
    fi
    mkdir -p "${home}"
    chmod 700 "${home}"
    gpg --homedir "${home}" --batch --yes --quiet \
        --pinentry-mode loopback --passphrase '' "$@"
}

# gentian_signing_uid <role> — the address a key is made for.
#
# The cluster is in the address, so two clusters' keys never look alike in a
# log, and the domain is the kernel domain because that is the name the
# cluster is actually known by.
gentian_signing_uid() {
    local role="$1"
    printf 'Gentian %s (%s) <gentian-%s@%s>' \
        "${role}" "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-cluster}" \
        "${role}" "${KERNEL_DOMAIN:-cluster.invalid}"
}

# gentian_signing_recorded_id <role> — the long key id the deployments
# repository records for this cluster's key of that role, or empty.
#
# clusters/<id>/kernel/signing/keys.env is where the first install wrote down
# which keys this cluster trusts. It needs the checkout's path and the cluster
# id and nothing else -- in particular not the kernel domain, which is read
# out of the claim later than the first thing that signs.
gentian_signing_recorded_id() {
    local var file id
    case "$1" in
        break-glass) var="GENTIAN_SIGNING_KEY_BREAK_GLASS" ;;
        director)    var="GENTIAN_SIGNING_KEY_DIRECTOR" ;;
        *)           return 0 ;;
    esac
    [[ -n "${GENTIAN_DEPLOYMENTS_PATH:-}" && -n "${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-}" ]] || return 0
    file="${GENTIAN_DEPLOYMENTS_PATH}/clusters/${GENTIAN_DEPLOYMENTS_CLUSTER_ID}/kernel/signing/keys.env"
    [[ -f "${file}" ]] || return 0
    id="$(awk -F= -v k="${var}" '$1 == k {print $2; exit}' "${file}" 2>/dev/null | tr -d '[:space:]' || true)"
    # A key id and nothing else: the file comes out of a repository, and what
    # it says is handed to gpg as the name of a key.
    [[ "${id}" =~ ^[0-9A-Fa-f]{16,40}$ ]] || return 0
    printf '%s' "${id}"
}

# _gpg_fingerprint <key id or uid> — the fingerprint of the one key the
# keyring holds under that name, or empty.
_gpg_fingerprint() {
    local listing
    listing="$(_gpg --list-keys --with-colons "$1" 2>/dev/null || true)"
    [[ -n "${listing}" ]] || return 0
    printf '%s' "$(printf '%s\n' "${listing}" | awk -F: '$1=="fpr" {print $10; exit}' || true)"
}

# gentian_signing_key_id <role> — the key's fingerprint, or empty if there is
# none. "No such key" is the ordinary answer the first time, not a failure:
# every guard here is written so the installer's ERR trap does not treat
# looking for something that is not there as an aborted install.
#
# The key the repository records for this cluster comes first, by its id.
#
# The lookup used to go by uid alone, and the uid has the kernel domain in it.
# Anything that asked before the domain had been read from the claim asked
# for "...@cluster.invalid", found nothing, and concluded this cluster had no
# key: a run then generated a second break-glass key beside the first and
# signed the cluster's definition with one nothing trusts. The recorded id
# does not depend on what has been read yet, and it is also the one statement
# of which key is THIS cluster's -- so a keyring that holds another key under
# a similar name, a stray one included, cannot be answered with.
#
# By uid only when the repository records nothing the keyring has -- the
# first install, before keys.env is written -- and then only once the kernel
# domain is known. Without it the uid is the placeholder address, which names
# no cluster's key.
gentian_signing_key_id() {
    local role="$1" recorded fpr
    recorded="$(gentian_signing_recorded_id "${role}")"
    if [[ -n "${recorded}" ]]; then
        fpr="$(_gpg_fingerprint "${recorded}")"
        if [[ -n "${fpr}" ]]; then
            printf '%s' "${fpr}"
            return 0
        fi
    fi
    [[ -n "${KERNEL_DOMAIN:-}" ]] || return 0
    _gpg_fingerprint "$(gentian_signing_uid "${role}")"
}

# gentian_ensure_signing_key <role> — make the key if this cluster has none.
#
# Idempotent: a second install run must not mint a second key, because the
# key id is written into the claim and into Argo CD's keyring, and a cluster
# that trusted two ids for one role would be a cluster where nobody could say
# which key was supposed to be able to write. "This cluster has one" is
# gentian_signing_key_id's answer -- the key the repository records, then the
# key made for this cluster's address.
gentian_ensure_signing_key() {
    local role="$1" id
    command -v gpg >/dev/null 2>&1 || {
        error "gpg is required to sign deployment commits (AD-2) and is not installed." >&2
        error "  Debian/Ubuntu: apt-get install gnupg" >&2
        return 1
    }
    id="$(gentian_signing_key_id "${role}")"
    if [[ -n "${id}" ]]; then
        printf '%s' "${id}"
        return 0
    fi
    # Never under --dry-run or --validate: a key is the most durable thing
    # this installer makes, and a run that promised to change nothing made one.
    if gentian_read_only; then
        gentian_would "generate the ${role} signing key, which this host's keyring does not hold" >&2
        return 1
    fi
    # Never without the kernel domain either. The key's address would be the
    # placeholder one, and the next run -- which does know the domain -- would
    # not recognise it as this cluster's.
    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        error "The ${role} signing key cannot be generated before the kernel domain is known." >&2
        return 1
    fi
    # The repository records a key for this role and this host does not have
    # it. Generating one is still how a host that never held the key gets to
    # sign, and what is published from here replaces the recorded id -- so it
    # is said, rather than left to be found in the log afterwards.
    local recorded; recorded="$(gentian_signing_recorded_id "${role}")"
    if [[ -n "${recorded}" ]]; then
        warn "The deployments repository records the ${role} key ${recorded} for this cluster," >&2
        warn "  and this host's keyring ($(gentian_gpg_home)) does not hold it. A new key is" >&2
        warn "  generated and will replace it in keys.env. To keep the recorded key instead," >&2
        warn "  stop here and restore it from the recovery kit: ./install.sh --recover <kit>" >&2
    fi
    _gpg --quick-generate-key "$(gentian_signing_uid "${role}")" ed25519 sign never \
        >/dev/null 2>&1 || {
        error "Could not generate the ${role} signing key." >&2
        return 1
    }
    id="$(gentian_signing_key_id "${role}")"
    [[ -n "${id}" ]] || { error "The ${role} key was generated and cannot be read back." >&2; return 1; }
    # stderr: the id is this function's value, and a log line on stdout would
    # become part of it.
    info "Generated the ${role} signing key ${id}." >&2
    printf '%s' "${id}"
}

# gentian_export_public_key <role> — the armoured public half.
#
# By the fingerprint the lookup answered with, not by uid: the key that is
# exported is then the key that was found, whatever address it was made for.
gentian_export_public_key() {
    local fpr; fpr="$(gentian_signing_key_id "$1")"
    [[ -n "${fpr}" ]] || return 1
    _gpg --armor --export "${fpr}"
}

# gentian_export_secret_key <role> — the armoured private half.
#
# Only the director's is ever exported: it has to reach OpenBao, and from
# there the pod. The break-glass key's private half stays on this host and in
# the recovery kit.
gentian_export_secret_key() {
    local fpr; fpr="$(gentian_signing_key_id "$1")"
    [[ -n "${fpr}" ]] || return 1
    _gpg --armor --export-secret-keys "${fpr}"
}

# gentian_git_sign_args <role> — the git flags that sign as this key.
#
# Passed per invocation rather than written into the repository's config: the
# deployments checkout belongs to the operator, and an installer that left
# commit.gpgsign behind would sign their unrelated commits too.
gentian_git_sign_args() {
    local id; id="$(gentian_signing_key_id "$1")"
    [[ -n "${id}" ]] || return 1
    # git runs gpg itself, so it needs the same homedir and the same loopback
    # answer this file uses everywhere else. A wrapper script rather than
    # flags, because gpg.program takes a program and not a command line.
    local wrapper; wrapper="$(gentian_gpg_home)/git-gpg"
    # Nothing signs under --dry-run or --validate, so nothing needs the wrapper.
    if [[ ! -x "${wrapper}" ]] && gentian_read_only; then
        return 1
    fi
    if [[ ! -x "${wrapper}" ]]; then
        mkdir -p "$(gentian_gpg_home)"
        cat > "${wrapper}" <<WRAP
#!/usr/bin/env bash
exec gpg --homedir "$(gentian_gpg_home)" --batch --yes \
    --pinentry-mode loopback --passphrase '' "\$@"
WRAP
        chmod 700 "${wrapper}"
    fi
    printf -- '-c gpg.program=%s -c user.signingkey=%s -c commit.gpgsign=true -c gpg.format=openpgp' \
        "${wrapper}" "${id}"
}

# gentian_signing_key_long_id <role> — the 16-hex form Argo CD wants.
#
# Two forms are in play and they are not interchangeable. gpg's
# user.signingkey takes the full 40-hex fingerprint; Argo CD's
# AppProject.spec.sourceIntegrity lists keys by the long key id, which is its
# last 16 — the same form `git log --format=%GK` prints, so what a reviewer
# reads out of the log is what the claim has to say.
gentian_signing_key_long_id() {
    local fpr; fpr="$(gentian_signing_key_id "$1")"
    [[ -n "${fpr}" ]] || return 0
    printf '%s' "${fpr: -16}"
}

# gentian_publish_signing_material <kernel-dir> — the public halves, in git.
#
# Both keys' public halves and their long ids go into the deployments
# repository beside the claims, committed with everything else. Three reasons
# they belong there rather than only in the cluster:
#
#   - A reviewer can see which keys this cluster trusts without cluster
#     access, which is the whole point of git being the audited source (AD-2).
#   - Argo CD's repo-server keyring is built FROM these files, so what the
#     cluster trusts and what the repository says it trusts cannot drift.
#   - A rebuilt cluster reads them back, so the key ids survive a purge
#     without anybody writing them down.
#
# The private halves are never here. The director's goes to OpenBao; the
# break-glass key's stays on the install host and in the recovery kit.
gentian_publish_signing_material() {
    local dir="$1/signing" role bg_id dir_id
    gentian_read_only && return 0
    mkdir -p "${dir}"

    for role in break-glass director; do
        gentian_ensure_signing_key "${role}" >/dev/null || return 1
        gentian_export_public_key "${role}" > "${dir}/${role}.asc" || {
            error "Could not export the ${role} public key."
            return 1
        }
    done

    bg_id="$(gentian_signing_key_long_id break-glass)"
    dir_id="$(gentian_signing_key_long_id director)"
    cat > "${dir}/keys.env" <<EOF
# The keys Argo CD will accept commits from on this repository (AD-2).
#
# Generated by install.sh; the public halves are the .asc files beside this
# one. These are LONG KEY IDS -- the last 16 hex of the fingerprint, which is
# the form AppProject.spec.sourceIntegrity takes and the form
# \`git log --format=%GK\` prints, so what a reviewer reads out of the log is
# what the cluster was told to trust.
#
# The director signs what it writes. The break-glass key signs what a person
# writes directly, which before the cluster exists is the only way the first
# claim can get here at all.
GENTIAN_SIGNING_KEY_DIRECTOR=${dir_id}
GENTIAN_SIGNING_KEY_BREAK_GLASS=${bg_id}
EOF
    info "Published signing material to clusters/.../kernel/signing (director ${dir_id}, break-glass ${bg_id})."
}

# gentian_signing_keys_from_deployment <kernel-dir> — the trusted ids, in order.
#
# Read from git rather than from the local keyring: the cluster must trust
# what the repository says, and an install host whose keyring has drifted from
# the repository is exactly the case worth catching rather than papering over.
gentian_signing_keys_from_deployment() {
    local env_file="$1/signing/keys.env" d b
    [[ -f "${env_file}" ]] || return 0
    d="$(awk -F= '$1=="GENTIAN_SIGNING_KEY_DIRECTOR"{print $2}' "${env_file}" || true)"
    b="$(awk -F= '$1=="GENTIAN_SIGNING_KEY_BREAK_GLASS"{print $2}' "${env_file}" || true)"
    printf '%s %s' "${d}" "${b}"
}

# gentian_import_break_glass_key <armoured> — put a kit's key back.
#
# The counterpart to what export_recovery_kit takes out. Ownertrust is set as
# well as the key imported: without it gpg signs happily and then reports its
# own signature as "good, but I do not know whether to trust this key", which
# is the difference between git printing G and U and is the kind of thing that
# looks like a broken key rather than a missing statement about its holder.
gentian_import_break_glass_key() {
    local armoured="$1" fpr
    [[ -n "${armoured}" ]] || return 1
    gentian_read_only && return 1
    command -v gpg >/dev/null 2>&1 || return 1
    printf '%s' "${armoured}" | _gpg --import >/dev/null 2>&1 || return 1
    fpr="$(gentian_signing_key_id break-glass)"
    [[ -n "${fpr}" ]] || return 1
    printf '%s:6:\n' "${fpr}" | _gpg --import-ownertrust >/dev/null 2>&1 || return 1
}
