#!/usr/bin/env bash
# =============================================================================
# scripts/lib/recovery.sh — export and re-import the bootstrap material
# =============================================================================
# Almost everything in a Gentian cluster is reconstructible: configuration from
# Git, sixteen kernel credentials from the master password and the salt, and
# every Secret from OpenBao by way of ESO. What is NOT reconstructible is the
# handful of values below — and losing them turns "rebuild from Git" from a plan
# into a hope.
#
# WHAT THE KIT DOES AND DOES NOT DO
#
# It makes derived credentials come back IDENTICAL. Install a fresh cluster with
# --recover and every database user, service account and app password derives to
# the value it had before. Without it you get a working cluster with entirely
# different credentials, which is a migration rather than a restore.
#
# It does NOT restore OpenBao. A fresh OpenBao initialises itself and issues new
# unseal material; the keys carried here belong to the OLD instance and are
# useful for exactly one thing — unsealing a restored Raft snapshot. Restoring
# that snapshot is a separate operation this does not perform.
# =============================================================================

[[ -n "${GENTIAN_RECOVERY_LOADED:-}" ]] && return 0
GENTIAN_RECOVERY_LOADED=1

GENTIAN_KIT_VERSION=1

# The only keys a kit may carry. Import walks this list rather than evaluating
# the file, so a tampered kit cannot introduce a variable the installer did not
# expect — the file is encrypted, but "encrypted" is not "trusted".
_KIT_KEYS=(
    KERNEL_DOMAIN
    GENTIAN_DEPLOYMENTS_REPO GENTIAN_DEPLOYMENTS_BRANCH
    GENTIAN_DEPLOYMENTS_CLUSTER_ID GENTIAN_DEPLOYMENTS_STAGE
    GENTIAN_OS_REPO GENTIAN_OS_BRANCH GENTIAN_OS_IMAGE_REPOSITORY
    MASTER_PASSWORD DERIVATION_SALT
    GENTIAN_DEPLOYMENTS_GIT_USERNAME GENTIAN_DEPLOYMENTS_GIT_TOKEN
    GENTIAN_OS_GIT_USERNAME GENTIAN_OS_GIT_TOKEN
    GENTIAN_APPS_GIT_USERNAME GENTIAN_APPS_GIT_TOKEN
    GENTIAN_UI_GIT_USERNAME GENTIAN_UI_GIT_TOKEN
    REGISTRY_USER REGISTRY_PASSWORD
    CF_API_TOKEN
    SMTP_RELAY_USERNAME SMTP_RELAY_PASSWORD
    TRANSIT_UNSEAL_KEY OPENBAO_RECOVERY_KEYS OPENBAO_ROOT_TOKEN
    BACKUP_AGE_IDENTITY
)

# =============================================================================
# Encryption
# =============================================================================
# age when it is present, because it authenticates: a tampered kit fails to
# decrypt rather than yielding plausible garbage. openssl otherwise, since it is
# already a prerequisite and adding one for a rarely-used command is a poor
# trade — but the difference is stated rather than hidden.

_kit_cipher() {
    if command -v age >/dev/null 2>&1; then echo age; else echo openssl; fi
}

_kit_encrypt() {
    local out="$1"
    case "$(_kit_cipher)" in
        age)
            if [[ -n "${GENTIAN_KIT_RECIPIENT:-}" ]]; then
                # The unattended path: a public key needs no terminal, so a
                # scheduled export works and only the private key holder reads it.
                age -r "${GENTIAN_KIT_RECIPIENT}" -o "${out}"
            else
                age -p -o "${out}"
            fi
            ;;
        openssl)
            warn "age not found; using openssl. The kit will be encrypted but NOT authenticated —"
            warn "  tampering yields garbage rather than an error. Install age for a stronger kit."
            warn "  This kit is NOT an age file: name it .enc, not .age."
            openssl enc -aes-256-cbc -pbkdf2 -iter 600000 -salt -out "${out}"
            ;;
    esac
}

_kit_decrypt() {
    local in="$1"
    # age's own header, so a kit is decrypted with whatever produced it rather
    # than with whatever happens to be installed now.
    if head -c 32 "${in}" 2>/dev/null | grep -q 'age-encryption'; then
        command -v age >/dev/null 2>&1 || {
            error "This kit is age-encrypted but age is not installed."
            return 1
        }
        # A kit written for a recipient needs that recipient's private key; one
        # written with -p needs only the passphrase age asks for.
        if [[ -n "${GENTIAN_KIT_IDENTITY:-}" ]]; then
            age -d -i "${GENTIAN_KIT_IDENTITY}" "${in}"
        else
            age -d "${in}"
        fi
    else
        openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -in "${in}"
    fi
}

# =============================================================================
# Gathering
# =============================================================================

# _kit_from_openbao <field> — read a field of the master-password path.
_kit_from_openbao() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 1
    bao kv get -mount=secret -field="$1" \
        gentian-os/kernel/internal/master-password 2>/dev/null
}

# _kit_from_json <file> <jq-filter>
_kit_from_json() {
    [[ -r "$1" ]] || return 1
    jq -r "$2 // empty" "$1" 2>/dev/null
}

# _kit_print_backup_key <identity-file> <recipient>
#
# The sheet to put in a safe. Printed once, at generation, because the identity
# is in the kit and nowhere else — there is no later command that can reproduce
# this without the kit already in hand.
#
# Paper is a real answer for a key of this shape, not a curiosity. An age
# identity is 74 characters of bech32: small enough for a QR at the highest
# error correction, and bech32 carries a checksum, so a character misread off
# paper is detected rather than silently wrong. That is the property that makes
# transcription survivable.
#
# The public key is on the sheet too. It is what lets someone hold this page
# against a running cluster and confirm it is the right one, without decrypting
# anything and without the cluster learning anything it does not already have.
_kit_print_backup_key() {
    local identity_file="$1" recipient="$2" identity cluster png
    identity="$(grep -m1 'AGE-SECRET-KEY-' "${identity_file}" 2>/dev/null || true)"
    [[ -n "${identity}" ]] || return 0
    cluster="${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-${KERNEL_DOMAIN:-unknown}}"
    png="gentian-backup-key-${cluster}.png"

    echo
    echo "  ┌───────────────────────────────────────────────────────────────┐"
    echo "  │  GENTIAN BACKUP KEY — PRINT IT, THEN STORE IT OFFLINE         │"
    echo "  └───────────────────────────────────────────────────────────────┘"
    echo
    echo "    Cluster : ${cluster}"
    echo "    Created : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    echo "    Public key (safe to share):"
    echo "      ${recipient}"
    echo
    echo "    IDENTITY — opens every backup. Treat it as the backup itself:"
    echo "      ${identity}"
    echo

    # The PNG is the copy that survives losing every computer. Written 0600
    # first, so it is never briefly world-readable with a private key in it.
    if command -v qrencode >/dev/null 2>&1; then
        if (umask 077 && qrencode -o "${png}" -l H -s 8 "${identity}" 2>/dev/null); then
            success "  QR code written to ${png}"
        else
            png=""
            warn "  Could not write the QR code; the identity above is the copy to keep."
        fi
        qrencode -t UTF8 -l H -m 2 "${identity}" 2>/dev/null || true
        echo
    else
        png=""
        warn "  qrencode is not installed, so no QR code was written."
        warn "  Install it (apt/brew/apk install qrencode) and run:"
        warn "    qrencode -o ${png:-backup-key.png} -l H -s 8 '<the identity above>'"
        echo
    fi

    echo "    WHAT TO DO NOW"
    echo "      1. Print ${png:-the identity above}."
    echo "      2. Put the paper where your break-glass material lives — a safe,"
    echo "         not this cluster and not the machine you would restore from."
    echo "      3. Delete the PNG from this disk once it is printed."
    echo
    echo "    Without it every backup is permanently unreadable. To use it:"
    echo "      age -d -i <key-file> manifest.json.age > manifest.json"
    echo
}

# _kit_recipient — the cluster's backup recipient, if it has one.
_kit_recipient() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 0
    bao kv get -mount=secret -field=recipients \
        gentian-os/kernel/backup/recipients 2>/dev/null || true
}

# _kit_backup_escrow_path — where an escrowed identity lives.
#
# Under kernel/, so the cluster-admin policy already reads it and no new grant
# is needed for the person who would be restoring. eso-read denies this exact
# path — see the Cluster composition — so escrow means "a cluster administrator
# can read it", not "the cluster can read it". The distinction is the whole
# point: a key ESO could materialise into a Secret would be readable by anything
# that takes the cluster, which is the one thing escrow must not come to mean.
_KIT_BACKUP_ESCROW_PATH="gentian-os/kernel/backup/identity"

# _kit_escrow_enabled — whether this cluster escrows the backup identity.
#
# On unless the cluster says otherwise, because the likelier disaster is a lost
# recovery kit rather than a stolen cluster, and a backup nobody can open is not
# a backup. Read from the live Cluster resource, which is where the claim in
# gentian-deployments lands, so the answer is the one committed in git rather
# than one supplied at the prompt.
#
# Only the literal "false" turns it off. The XRD defaults the field to true, so
# a current cluster always states it either way and the empty case means the
# resource could not be read — an XRD that predates this field, no cluster
# access, a kit exported from elsewhere. Empty follows the documented default
# rather than quietly inverting it, and _kit_backup_identity says which way it
# went so the choice is visible in the transcript rather than inferred.
#
# The narrow risk this accepts: a claim that says false, on a cluster whose XRD
# is too old to know the field, reads as empty and escrows anyway. That window
# closes the moment the XRD is applied, and GENTIAN_BACKUP_ESCROW_IDENTITY=false
# forces the answer meanwhile.
_kit_escrow_enabled() {
    if [[ -n "${GENTIAN_BACKUP_ESCROW_IDENTITY:-}" ]]; then
        [[ "${GENTIAN_BACKUP_ESCROW_IDENTITY}" != "false" ]]
        return
    fi
    local value
    value="$(kubectl get cluster.gentianos.io -A \
        -o jsonpath='{.items[0].spec.backup.escrowIdentity}' 2>/dev/null || true)"
    [[ "${value}" != "false" ]]
}

# _kit_escrow_stated — whether the cluster actually answered, or we defaulted.
#
# Reported rather than hidden: "escrowed because the cluster asked" and
# "escrowed because nothing could be read" are the same outcome and very
# different facts, and only one of them is a decision somebody made.
_kit_escrow_stated() {
    [[ -n "${GENTIAN_BACKUP_ESCROW_IDENTITY:-}" ]] && return 0
    local value
    value="$(kubectl get cluster.gentianos.io -A \
        -o jsonpath='{.items[0].spec.backup.escrowIdentity}' 2>/dev/null || true)"
    [[ -n "${value}" ]]
}

# _kit_escrowed_identity — the identity OpenBao holds, when escrow is on.
_kit_escrowed_identity() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 0
    bao kv get -mount=secret -field=identity \
        "${_KIT_BACKUP_ESCROW_PATH}" 2>/dev/null || true
}

# _kit_backup_identity — the cluster's own age key pair for bundle encryption.
#
# The public half goes into OpenBao, where the operator reads it; the private
# half goes into this kit and nowhere else. That split is the whole point: a key
# stored on the cluster it protects is readable by whoever takes that cluster,
# which is precisely the situation backups exist for.
#
# Generated here rather than by hand because "Platform key" is the documented
# default and the only mode a schedule can use — a passphrase has nobody to type
# it at 03:00 — and nothing generated one. A cluster nobody had hand-edited
# therefore had no key at all, and its nightly backup failed every night with a
# message telling an administrator to go and configure one. The kit is the right
# moment: it is already the step whose output must be kept off the cluster.
#
# Never regenerates. A second pair would orphan every bundle written to the
# first, silently, and the symptom would be an unreadable backup during the
# incident that needed it. If OpenBao already holds a recipient and this run
# cannot supply the matching identity, the kit goes out without it and says so —
# the earlier kit is then the only copy, and that is worth knowing loudly.
_kit_backup_identity() {
    [[ -n "${BAO_TOKEN:-}" ]] || return 0

    local existing
    existing="$(_kit_recipient)"

    if [[ -n "${existing}" ]]; then
        if [[ -n "${BACKUP_AGE_IDENTITY:-}" ]]; then
            return 0
        fi
        # Escrow's first payoff, and the reason it is worth having beyond a
        # restore: the identity can be fetched, so a second kit carries it too.
        # Without escrow this is the point at which the earlier kit becomes
        # irreplaceable, and every kit written afterwards is missing the one
        # value that cannot be regenerated.
        local escrowed=""
        if _kit_escrow_enabled; then
            escrowed="$(_kit_escrowed_identity)"
        fi
        if [[ -n "${escrowed}" ]]; then
            BACKUP_AGE_IDENTITY="${escrowed}"
            success "  Backup identity read from OpenBao escrow; this kit carries it."
            return 0
        fi
        warn "  This cluster already has a backup recipient (${existing:0:16}...)."
        warn "  Its identity is not available here, so this kit cannot carry it."
        warn "  The kit that created it remains the only copy — keep it."
        if _kit_escrow_enabled; then
            warn "  Escrow is on but OpenBao holds no identity: the key predates it."
            warn "  Supply it once to close the gap:"
            warn "    bao kv put -mount=secret ${_KIT_BACKUP_ESCROW_PATH} identity=@identity.txt"
        fi
        return 0
    fi

    # Refused, not warned. This is the only thing that creates the cluster's
    # backup key, and a warning here produced exactly the outcome it described:
    # a kit exported, "recoveryKitExported = true" recorded, no key anywhere,
    # and a nightly export failing with "no age recipients configured" until
    # somebody went looking. Pre-flight now requires age, so reaching this means
    # the export was run standalone on a machine without it.
    #
    # Non-zero, so the caller fails rather than continuing to write a kit that
    # is missing the one value it was re-run to produce.
    if ! command -v age-keygen >/dev/null 2>&1; then
        error "  age-keygen not found, and this cluster has no backup key."
        error ""
        error "  It is the only thing that can make one, and every scheduled export"
        error "  encrypts to it. Writing a kit now would record a successful export"
        error "  and leave the nightly backup failing with \"no age recipients"
        error "  configured\"."
        error "    Debian/Ubuntu   sudo apt install age"
        error "    macOS           brew install age"
        error "    Alpine          apk add age"
        error "  Then run this again."
        return 1
    fi

    # A directory, not mktemp's file: age-keygen -o refuses to write over a
    # path that already exists, and mktemp creates the file it names.
    local dir tmp recipient
    dir="$(mktemp -d)"
    chmod 700 "${dir}"
    tmp="${dir}/identity.txt"
    if ! age-keygen -o "${tmp}" >/dev/null 2>&1; then
        warn "  Could not generate a backup key pair."
        rm -rf "${dir}"
        return 0
    fi
    recipient="$(age-keygen -y "${tmp}" 2>/dev/null || true)"
    if [[ -z "${recipient}" ]]; then
        rm -rf "${dir}"
        return 0
    fi

    # The public half first. If this fails there is no key on the cluster and no
    # identity in the kit, which is the state we started in — better than a kit
    # holding an identity for a recipient nothing uses.
    if ! bao kv put -mount=secret gentian-os/kernel/backup/recipients \
        "recipients=${recipient}" >/dev/null 2>&1; then
        warn "  Could not store the backup recipient in OpenBao; no key configured."
        rm -rf "${dir}"
        return 0
    fi

    BACKUP_AGE_IDENTITY="$(cat "${tmp}")"

    # The private half, only when the cluster definition asks for it. Written
    # after the recipient rather than before: a cluster whose escrow write fails
    # still has a working key and a kit that carries it, which is the default
    # arrangement — the reverse would leave an identity escrowed for a recipient
    # nothing encrypts to.
    if _kit_escrow_enabled; then
        if bao kv put -mount=secret "${_KIT_BACKUP_ESCROW_PATH}" \
            "identity=${BACKUP_AGE_IDENTITY}" >/dev/null 2>&1; then
            success "  Backup key generated. Public half and escrowed identity in OpenBao."
            if _kit_escrow_stated; then
                warn "  This cluster escrows the backup identity (spec.backup.escrowIdentity)."
            else
                warn "  Escrowed by default: this cluster's definition could not be read, so"
                warn "  the documented default applied. Set spec.backup.escrowIdentity to say"
                warn "  so deliberately, or false to keep the key in this kit alone."
            fi
            warn "  A cluster administrator can restore without this kit — and anyone who"
            warn "  reaches OpenBao as one can read every bundle. Keep the kit anyway: it"
            warn "  is the only copy that survives losing the cluster."
        else
            warn "  Could not escrow the identity in OpenBao; it is in this kit only."
            warn "  This kit is therefore the only copy. Keep it, and fix the escrow write."
        fi
    else
        success "  Backup key generated. Public half in OpenBao, identity in this kit."
    fi

    _kit_print_backup_key "${tmp}" "${recipient}"
    rm -rf "${dir}"

    # Pin it where the cluster cannot rewrite it. OpenBao is what makes the
    # default work untouched; git is what stops a compromised cluster pointing
    # future backups at somebody else's key, because the operator prefers the
    # pinned value and reports a disagreement.
    warn "  Add this to clusters/<cluster>/kernel/values.yaml in gentian-deployments:"
    warn "    backupRecipients:"
    warn "      - ${recipient}"
}

# _kit_gather — fill the exportable variables from whatever source has them.
#
# Every value has at least two sources because the interesting case is a
# half-broken cluster: the environment from a run in progress, OpenBao when it
# is reachable, and the init files the installer wrote.
_kit_gather() {
    MASTER_PASSWORD="${MASTER_PASSWORD:-$(_kit_from_openbao value || true)}"
    DERIVATION_SALT="${DERIVATION_SALT:-$(_kit_from_openbao salt || true)}"

    # Both spellings, because the field name is OpenBao's and not ours.
    # `bao operator init -format=json` writes unseal_keys_base64 /
    # recovery_keys_base64; the _b64 spellings below are what older builds and
    # Vault's own docs use. Reading only one meant the value was silently
    # absent — see the recovery-key case just below, which is the reason this
    # matters.
    TRANSIT_UNSEAL_KEY="${TRANSIT_UNSEAL_KEY:-$(
        _kit_from_json "${TRANSIT_INIT_FILE:-${HOME}/.gentian/openbao-transit-init.json}" \
            '(.unseal_keys_base64 // .unseal_keys_b64 // [])[0]' || true)}"
    if [[ -z "${TRANSIT_UNSEAL_KEY}" ]]; then
        TRANSIT_UNSEAL_KEY="$(kubectl get secret openbao-transit-unseal -n openbao \
            -o jsonpath='{.data.unseal-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    fi

    # recovery_keys_base64 first — that is what the init file on disk actually
    # contains. This read asked only for recovery_keys_b64, which is not a
    # field OpenBao writes, so it evaluated to empty every time and the kit
    # went out with OPENBAO_RECOVERY_KEYS listed under "Absent" on every
    # export ever taken. The recovery key is the one credential in the kit
    # that cannot be re-derived from anything else: it exists in this file and
    # nowhere else, and E-04 deletes the file. A kit without it is a kit that
    # cannot open OpenBao when Keycloak is what broke.
    OPENBAO_RECOVERY_KEYS="${OPENBAO_RECOVERY_KEYS:-$(
        _kit_from_json "${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}" \
            '(.recovery_keys_base64 // .recovery_keys_b64 // []) | join(",")' || true)}"
    OPENBAO_ROOT_TOKEN="${OPENBAO_ROOT_TOKEN:-${BAO_TOKEN:-$(
        _kit_from_json "${OPENBAO_INIT_FILE:-${HOME}/.gentian/openbao-init.json}" '.root_token' || true)}}"

    _kit_backup_identity || return 1

    if [[ -z "${KERNEL_DOMAIN:-}" ]]; then
        KERNEL_DOMAIN="$(kubectl get cluster.gentianos.io -n crossplane-system \
            -o jsonpath='{.items[0].spec.kernelDomain}' 2>/dev/null || true)"
    fi
}

# =============================================================================
# Export
# =============================================================================

# The default name follows the cipher actually used. Naming an openssl kit .age
# produces a file `age` refuses to open, and the person who reaches for it is by
# definition rebuilding a cluster from nothing.
_kit_default_path() {
    case "$(_kit_cipher)" in
        age) echo "gentian-recovery-kit.age" ;;
        *)   echo "gentian-recovery-kit.enc" ;;
    esac
}

export_recovery_kit() {
    local out="${1:-$(_kit_default_path)}"

    banner "Recovery kit"
    _kit_gather || return 1

    # The salt is the reason this command exists. It lives only in OpenBao, so a
    # disaster that loses OpenBao's storage loses it too — and then the master
    # password alone reproduces nothing.
    if [[ -z "${MASTER_PASSWORD:-}" || -z "${DERIVATION_SALT:-}" ]]; then
        error "Cannot export: the master password and derivation salt are both required."
        error "  Neither the environment nor OpenBao supplied them."
        error ""
        error "  The export tries an OIDC sign-in as cluster-admin when it has no token,"
        error "  so reaching this means that did not complete either — no browser, the"
        error "  sign-in was cancelled, or OpenBao is unreachable from here."
        error ""
        error "  Supply a token directly:"
        error "    export BAO_ADDR=https://localhost:8200   # localhost is in the cert SANs"
        error "    bao login -method=oidc                   # your Keycloak identity"
        error "    export BAO_TOKEN=\"\$(bao print token)\""
        error ""
        error "  Or run this while the values are still in a live install shell."
        return 1
    fi

    local body key value present=() missing=()
    body=""
    body+="# gentian-os recovery kit"$'\n'
    body+="GENTIAN_KIT_VERSION=${GENTIAN_KIT_VERSION}"$'\n'
    for key in "${_KIT_KEYS[@]}"; do
        value="${!key:-}"
        if [[ -z "${value}" ]]; then missing+=("${key}"); continue; fi
        present+=("${key}")
        # base64 so a value containing newlines, quotes or shell metacharacters
        # survives the round trip without any quoting rules to get wrong.
        body+="${key}=$(printf '%s' "${value}" | openssl base64 -A)"$'\n'
    done

    # Refuse to replace a kit that holds a key this one does not.
    #
    # Every other value here is re-read on each run — the master password and
    # salt from OpenBao, the tokens from the init files — so overwriting was
    # always safe, and install(1) overwrites unconditionally at a fixed default
    # name. The backup identity is the first value in a kit that exists nowhere
    # else unless this cluster escrows it: without escrow OpenBao holds only its
    # public half, deliberately.
    #
    # So a second export from a shell that does not have the identity would
    # write a kit without it over the kit that had it, while printing "the
    # earlier kit is the only copy — keep it". That is the whole key to every
    # scheduled bundle, gone, with a reassuring message.
    #
    # _kit_gather has already run, and on an escrowing cluster it fetched the
    # identity, so BACKUP_AGE_IDENTITY is set and this does not fire. That is
    # the guard working, not being bypassed: the kit about to be written carries
    # the same key as the one it replaces. It still fires on an escrowing
    # cluster whose escrow is empty or unreadable, which is exactly when the
    # earlier kit is again the only copy.
    if [[ -z "${BACKUP_AGE_IDENTITY:-}" && -e "${out}" && -n "$(_kit_recipient)" ]]; then
        error "Refusing to overwrite ${out}."
        error "  This cluster has a backup recipient, so a kit exists that carries the"
        error "  matching identity — and this run does not have it, so the kit it would"
        error "  write would not either. Overwriting would destroy the only copy of the"
        error "  key that opens every scheduled bundle."
        error "  Either supply it:      export BACKUP_AGE_IDENTITY=\"\$(...from the existing kit...)\""
        error "  or write elsewhere:    ./install.sh --export-recovery-kit /path/to/new-kit.age"
        if _kit_escrow_enabled; then
            error "  This cluster sets spec.backup.escrowIdentity, but OpenBao holds no"
            error "  identity at ${_KIT_BACKUP_ESCROW_PATH} — the key predates escrow, or"
            error "  BAO_TOKEN cannot read it."
        fi
        return 1
    fi

    local tmp; tmp="$(mktemp)"
    if ! printf '%s' "${body}" | _kit_encrypt "${tmp}"; then
        rm -f "${tmp}"
        error "Encryption failed; no kit was written."
        if [[ "$(_kit_cipher)" == "age" && -z "${GENTIAN_KIT_RECIPIENT:-}" ]] && [[ ! -t 0 ]]; then
            error "  age -p reads its passphrase from the terminal, so it cannot run unattended."
            error "  For a scheduled export set GENTIAN_KIT_RECIPIENT to an age public key."
        fi
        return 1
    fi
    # Unguarded before, so a destination whose parent directory does not exist
    # failed with install(1)'s own raw message — "cannot create regular file
    # ...: No such file or directory" — and, under this file's set -e, aborted
    # the whole call right there. That is correct in outcome (no kit written,
    # nothing recorded as exported) but wrong in every other way: it names the
    # wrong command, gives no next step, and leaves the encrypted temp file
    # behind uncleaned. install(1) has no -p; the parent has to exist already.
    if ! install -m 0600 "${tmp}" "${out}"; then
        rm -f "${tmp}"
        error "Could not write ${out}."
        error "  install(1) failed — the most common reason is that the"
        error "  directory it lives in does not exist yet. Create it first,"
        error "  or point --export-recovery-kit at a path that already does."
        return 1
    fi
    rm -f "${tmp}"

    success "Wrote ${out} (${#present[@]} values, mode 0600, $(_kit_cipher))."
    info "  Included: ${present[*]}"
    [[ ${#missing[@]} -gt 0 ]] && warn "  Absent:   ${missing[*]}"

    # Said first, said plainly, and said as an instruction.
    #
    # What was here described the kit's confidentiality risk — that it grants
    # every derived credential to whoever decrypts it — and left the operator
    # to infer the availability one. They are not the same warning and the
    # second is the one that ends clusters: a leaked kit is a serious
    # incident, a lost kit is a cluster that cannot be rebuilt as itself. The
    # file is also, at this moment, sitting wherever the command was run from,
    # which for a bare default path is whatever directory the operator
    # happened to be in.
    echo ""
    warn "╔══════════════════════════════════════════════════════════════════╗"
    warn "║  MOVE THIS FILE SOMEWHERE SAFE, NOW                              ║"
    warn "╚══════════════════════════════════════════════════════════════════╝"
    warn "  ${out}"
    echo ""
    warn "  Without it this cluster CANNOT be rebuilt as itself. The derivation"
    warn "  salt and the OpenBao recovery key exist in this file and in this"
    warn "  cluster — nowhere else. Lose both and every credential a rebuild"
    warn "  derives comes out different, and there is no way back into OpenBao"
    warn "  if its login path ever breaks."
    echo ""
    warn "  It is also worth exactly that much to an attacker: it grants every"
    warn "  derived credential in the cluster to anyone who can decrypt it."
    warn "  Put it where your break-glass material already lives — a password"
    warn "  manager, a sealed vault, offline media. Not this directory, not a"
    warn "  git repository, not a home directory that is only on this machine."
    echo ""
    info "  Verify you can read it back before you rely on it:"
    if [[ "$(_kit_cipher)" == "age" ]]; then
        info "    age -d ${out} | head -1"
    else
        info "    openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -in ${out} | head -1"
    fi
    echo ""
    info "  The kit reproduces derived credentials on a rebuild. It does NOT"
    info "  restore OpenBao: a fresh instance issues its own unseal material, and"
    info "  the keys here belong to the old one — they unseal a restored Raft"
    info "  snapshot, nothing else."

    _record_kit_export_proof "${out}"
}

# _record_kit_export_proof — note in gentian-handover that a kit now exists.
#
# E-04 reads this before revoking the bootstrap token: once that token is
# gone, this kit — or another export after it — is the only way back into a
# cluster whose normal login path turns out to be broken. Revoking on the
# strength of "the operator probably ran this command at some point" is the
# same mistake WritePathProven replaced an inference with an observation for.
#
# Best-effort. A kit that wrote to disk but failed to record itself here is
# still a usable kit — the record is a convenience for the gate, not a
# property of the file — so this warns rather than fails the export.
_record_kit_export_proof() {
    local out="${1:-}"
    local ns="${GENTIAN_SYSTEM_NAMESPACE:-gentian-system}"
    local now; now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

    # The path is recorded too, so a later step can name the file rather than
    # describe it. E-04 stops the install to ask for two things, one of which
    # is "move that kit" — and an instruction to move a file is no use without
    # the file's name. It was written and announced hundreds of lines earlier,
    # which by then has scrolled away.

    # patch first: unambiguous (RFC 7396 JSON Merge Patch merges .data by key,
    # never wipes what handover.RecordWritePathProven or E-04 already wrote),
    # and correct on every re-run after the first. Only fails when the
    # ConfigMap does not exist yet — expected when export runs before anyone
    # has signed in, which GETTING-STARTED's own ordering invites.
    if kubectl patch configmap gentian-handover -n "${ns}" --type=merge \
        -p "{\"data\":{\"recoveryKitExported\":\"true\",\"recoveryKitExportedAt\":\"${now}\",\"recoveryKitPath\":\"${out}\"}}" \
        >/dev/null 2>&1; then
        return 0
    fi

    # Plain create, not apply: fires only when the patch above proved nothing
    # is there, so there is no existing .data to merge with and no ambiguity
    # about clobbering it. If something raced this (a sign-in landing in the
    # same instant) the create fails closed with AlreadyExists and this warns
    # rather than silently losing the exchange record.
    if kubectl create configmap gentian-handover -n "${ns}" \
        --from-literal="recoveryKitExported=true" \
        --from-literal="recoveryKitExportedAt=${now}" \
        --from-literal="recoveryKitPath=${out}" \
        >/dev/null 2>&1; then
        return 0
    fi

    warn "Could not record the export in ${ns}/gentian-handover — E-04 will not"
    warn "  see this kit until it can. Check kubectl access to that namespace,"
    warn "  or run this export again once it is available."
}

# =============================================================================
# Import
# =============================================================================

load_recovery_kit() {
    local kit="$1"
    [[ -r "${kit}" ]] || { error "Cannot read recovery kit: ${kit}"; return 1; }

    banner "Recovering from ${kit##*/}"
    local plain
    plain="$(_kit_decrypt "${kit}")" || { error "Could not decrypt ${kit}."; return 1; }

    case "${plain}" in
        *"gentian-os recovery kit"*) ;;
        *) error "That does not look like a recovery kit."; return 1 ;;
    esac

    local line key b64 loaded=()
    while IFS= read -r line; do
        case "${line}" in ''|'#'*) continue ;; esac
        key="${line%%=*}"; b64="${line#*=}"
        [[ "${key}" == "GENTIAN_KIT_VERSION" ]] && continue

        # Whitelist, not evaluation. The file is encrypted, which is not the
        # same as trusted, and sourcing it would let a key name decide what the
        # installer sets.
        local known=0 k
        for k in "${_KIT_KEYS[@]}"; do [[ "$k" == "${key}" ]] && known=1 && break; done
        if [[ ${known} -eq 0 ]]; then
            warn "Ignoring unknown key in kit: ${key}"
            continue
        fi

        # Values already in the environment win, so a deliberate override on the
        # command line is not silently undone by the kit.
        if [[ -n "${!key:-}" ]]; then continue; fi
        printf -v "${key}" '%s' "$(printf '%s' "${b64}" | openssl base64 -d -A 2>/dev/null)"
        export "${key?}"
        loaded+=("${key}")
    done <<< "${plain}"

    success "Loaded ${#loaded[@]} values: ${loaded[*]}"
    [[ -n "${KERNEL_DOMAIN:-}" ]] && info "  Cluster: ${KERNEL_DOMAIN}"
    echo ""
    info "Derived credentials will reproduce their original values."
    if [[ -n "${OPENBAO_ROOT_TOKEN:-}" ]]; then
        info "The kit carries OpenBao unseal material for the PREVIOUS instance. A fresh"
        info "install initialises its own; those keys matter only if you are also"
        info "restoring a Raft snapshot, which this does not do."
    fi
}
