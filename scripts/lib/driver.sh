#!/usr/bin/env bash
# =============================================================================
# scripts/lib/driver.sh — step discovery and execution
# =============================================================================
# The installer is a driver over scripts/steps/*.sh. Each step file is
# self-contained and declares a contract in its header:
#
#   # step: A-05-cert-manager
#   # phase: control-plane
#   # requires: A-03-namespaces
#   # provides: cert-manager, ClusterIssuers
#   # mutates: cluster-scoped CRDs, namespace cert-manager
#
# and implements up to three verbs:
#
#   check()   read-only. 0 = already satisfied, 1 = work to do,
#             2 = undefined (not applicable, or not yet answerable).
#   apply()   make it so. Idempotent.
#   destroy() reverse it. Idempotent.
#
# A check() that guards on config it needs — a feature flag, a token, a routing
# mode — returns UNDEFINED when that guard fails, never SATISFIED. Both skip on
# the forward pass, so the distinction costs nothing there; it exists so
# --status can tell "verified present" apart from "never asked".
#
# check() is the load-bearing verb: it reads cluster state and answers whether
# the step's `provides:` already holds. Because state is read from the cluster
# rather than from a local file, a re-run converges and there is no install
# state to keep on disk.
#
# One driver, three directions:
#
#   install.sh              forward; skip where check() is satisfied
#   install.sh --update     identical — convergence IS update
#   install.sh --uninstall  reverse order, destroy() each step
#
# check() must not mutate. --dry-run relies on it: the driver runs every
# check() and prints what apply() would do without calling apply() at all.
#
# Steps are numbered <PHASE>-<NN>: the letter groups them, the number orders
# them WITHIN that group. Inserting a step therefore only ever affects its own
# phase — there is always headroom, because each phase has its own sequence.
# A flat sequence does not have that property: by the time nine steps sit
# between 10 and 17 there is nowhere left to insert without a suffix.
#
# The `# phase:` header carries the readable name for the letter and drives
# --phase selection.
# =============================================================================

[[ -n "${GENTIAN_DRIVER_LOADED:-}" ]] && return 0
GENTIAN_DRIVER_LOADED=1

GENTIAN_STEPS_DIR="${GENTIAN_STEPS_DIR:-${SCRIPT_DIR}/scripts/steps}"

# Driver state. Parallel arrays rather than associative arrays: macOS ships
# bash 3.2, which has no `declare -A` (see docs/plans §7, portability).
_STEP_IDS=()
_STEP_FILES=()

# Set around each step so gentian_report_abort can name the step file to open.
export GENTIAN_CURRENT_STEP=""

# ─── Runtime flags, all driven by install.sh's argument parser ───────────────
GENTIAN_DRY_RUN="${GENTIAN_DRY_RUN:-0}"
GENTIAN_EXPLAIN="${GENTIAN_EXPLAIN:-0}"
GENTIAN_ONLY="${GENTIAN_ONLY:-}"
GENTIAN_FROM="${GENTIAN_FROM:-}"
GENTIAN_UNTIL="${GENTIAN_UNTIL:-}"
GENTIAN_SKIP="${GENTIAN_SKIP:-}"
GENTIAN_PHASE="${GENTIAN_PHASE:-}"

# =============================================================================
# Discovery
# =============================================================================

# step_id_of <file> — "scripts/steps/30-cert-manager.sh" → "30-cert-manager"
step_id_of() {
    local base
    base="$(basename "$1")"
    echo "${base%.sh}"
}

# step_number_of <id> — "B-03-openbao-init" → "B-03"
step_number_of() {
    echo "${1%%-*}-$(echo "$1" | cut -d- -f2)"
}

# step_label_of <id> — "B-03-openbao-init" → "openbao-init". Two segments to
# strip now, not one: the phase letter and the number.
step_label_of() {
    local t="${1#*-}"
    echo "${t#*-}"
}

# step_ordinal_of <id> — a sortable key for --from/--until: "B-03" → "B03".
#
# Compared as a STRING, not arithmetic. Phases order by letter and steps by a
# zero-padded number within the phase, so byte order is the execution order —
# which is the same property the filenames rely on.
step_ordinal_of() {
    local n; n="$(step_number_of "$1")"
    echo "${n//-/}"
}

# discover_steps — populate _STEP_IDS/_STEP_FILES in numeric filename order.
# Ordering is the filename prefix, so the sequence is visible from `ls scripts/steps/`
# without reading any code.
discover_steps() {
    _STEP_IDS=()
    _STEP_FILES=()

    if [[ ! -d "${GENTIAN_STEPS_DIR}" ]]; then
        error "Steps directory not found: ${GENTIAN_STEPS_DIR}"
        return 1
    fi

    local f
    # LC_ALL=C is load-bearing: locale-aware collation ignores punctuation and
    # would reorder these. In the C locale bytes decide, and because the phase is
    # a single letter and the number is zero-padded, byte order IS execution
    # order — A-01 … A-10, then B-01.
    while IFS= read -r f; do
        [[ -n "$f" ]] || continue
        _STEP_IDS+=("$(step_id_of "$f")")
        _STEP_FILES+=("$f")
    done < <(find "${GENTIAN_STEPS_DIR}" -maxdepth 1 -name '[A-Z]-[0-9][0-9]-*.sh' | LC_ALL=C sort)

    if [[ ${#_STEP_IDS[@]} -eq 0 ]]; then
        error "No step files found in ${GENTIAN_STEPS_DIR}"
        return 1
    fi

    # A misspelled --phase would otherwise select nothing and report success,
    # which reads exactly like "there was nothing to do".
    if [[ -n "${GENTIAN_PHASE}" ]]; then
        local known letters
        known="$(step_phases)"
        letters="$(printf '%s\n' "${_STEP_IDS[@]}" | cut -d- -f1 | sort -u)"
        if ! printf '%s\n%s\n' "${known}" "${letters}" | grep -qx "${GENTIAN_PHASE}"; then
            error "Unknown phase: ${GENTIAN_PHASE}"
            error "  Known phases: $(printf '%s' "${known}" | tr '\n' ' ')"
            error "  or by letter:  $(printf '%s' "${letters}" | tr '\n' ' ')"
            return 1
        fi
    fi
}

# step_phases — the declared phases, in execution order.
step_phases() {
    local i ph seen=""
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        ph="$(step_header "${_STEP_FILES[$i]}" phase)"
        [[ -n "${ph}" ]] || continue
        [[ " ${seen} " == *" ${ph} "* ]] && continue
        seen+="${ph} "
        echo "${ph}"
    done
}

# _file_for <id> — the step file backing an id.
_file_for() {
    local i
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        [[ "${_STEP_IDS[$i]}" == "$1" ]] && { echo "${_STEP_FILES[$i]}"; return 0; }
    done
    return 1
}

# step_header <file> <field> — read a `# field: value` contract line.
step_header() {
    sed -n "s/^# $2:[[:space:]]*//p" "$1" | head -1
}

# =============================================================================
# Selection
# =============================================================================

# _in_csv <needle> <csv> — substring-safe membership test on a comma list.
_in_csv() {
    local needle="$1" csv="$2" item
    local IFS=','
    for item in $csv; do
        # Match either the full id or its number, so `--only B-03` and
        # `--only B-03-openbao-init` both work.
        [[ "$item" == "$needle" || "$item" == "$(step_number_of "$needle")" ]] && return 0
    done
    return 1
}

# step_selected <id> — apply --phase / --only / --from / --until / --skip.
step_selected() {
    local id="$1"

    # Phase is a grouping, deliberately NOT part of the ordering key. A step
    # moves between phases by editing one header line; its number and its
    # neighbours' numbers do not change. Encoding the phase into the number
    # would make every re-grouping a renumbering — the problem the numbering
    # gaps and letter suffixes exist to avoid.
    if [[ -n "${GENTIAN_PHASE}" ]]; then
        # Accept either the readable name or the letter: --phase secrets and
        # --phase B select the same steps, because both appear in the numbering
        # an operator is reading off the screen.
        local ph letter
        ph="$(step_header "$(_file_for "${id}")" phase)"
        letter="${id%%-*}"
        [[ "${ph}" == "${GENTIAN_PHASE}" || "${letter}" == "${GENTIAN_PHASE}" ]] || return 1
    fi

    if [[ -n "${GENTIAN_ONLY}" ]]; then
        _in_csv "$id" "${GENTIAN_ONLY}" || return 1
    fi
    if [[ -n "${GENTIAN_SKIP}" ]] && _in_csv "$id" "${GENTIAN_SKIP}"; then
        return 1
    fi
    # String comparison would order "9" after "10"; both are zero-padded, so
    # 10# forces base-10 and avoids octal on values like 08.
    local ord; ord="$(step_ordinal_of "$id")"
    if [[ -n "${GENTIAN_FROM}" ]] && [[ "${ord}" < "$(step_ordinal_of "${GENTIAN_FROM}")" ]]; then
        return 1
    fi
    if [[ -n "${GENTIAN_UNTIL}" ]] && [[ "${ord}" > "$(step_ordinal_of "${GENTIAN_UNTIL}")" ]]; then
        return 1
    fi
    return 0
}

# =============================================================================
# Reporting
# =============================================================================

# Printed once when the phase changes, so a long run reads as five stages
# rather than thirty-five uncounted steps.
_GENTIAN_LAST_PHASE=""
_phase_banner() {
    local ph="$1"
    [[ -n "${ph}" && "${ph}" != "${_GENTIAN_LAST_PHASE}" ]] || return 0
    _GENTIAN_LAST_PHASE="${ph}"
    echo ""
    echo -e "${CYAN}── ${ph} ───────────────────────────────────────────${NC}"
}

_step_banner() {
    local id="$1" file="$2" provides
    _phase_banner "$(step_header "$file" phase)"
    provides="$(step_header "$file" provides)"
    echo ""
    echo -e "${CYAN}[$(step_number_of "$id")]${NC} $(step_label_of "$id")"
    [[ -n "$provides" ]] && echo "     provides: ${provides}"
    return 0
}

# gentian_run — announce a mutating command, then run it (or not, under
# --dry-run). Steps use this for the commands an operator most wants to see.
# It is deliberately not a general wrapper: a step that pipes or captures
# output should call the command directly.
gentian_run() {
    echo "     + $*"
    if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
        return 0
    fi
    "$@"
}

# =============================================================================
# Execution
# =============================================================================

# _load_step <file> — source a step in a clean slate so a verb left over from
# the previous step can never be silently reused.
_load_step() {
    unset -f check apply destroy 2>/dev/null || true
    # shellcheck source=/dev/null
    source "$1"
}

# _has_verb <name>
_has_verb() {
    declare -F "$1" >/dev/null 2>&1
}

# _step_verdict — run the loaded step's check() and return one of the CHECK_*
# codes. Call as: `verdict=0; _step_verdict || verdict=$?`.
#
# A step with no check() is UNDEFINED rather than MISSING: it has not told us
# there is work to do, only that it has no way to say. The callers decide what
# that means for their direction — the forward pass still applies such a step.
_step_verdict() {
    local rc=0
    _has_verb check || return "${CHECK_UNDEFINED}"
    check || rc=$?
    case "$rc" in
        0) return "${CHECK_SATISFIED}" ;;
        2) return "${CHECK_UNDEFINED}" ;;
        3) return "${CHECK_ALWAYS}" ;;
        # Any other nonzero — including a check() that died on `set -e` or a
        # kubectl that could not reach the API — is work to do. Guessing
        # "undefined" there would silently skip a step that was never verified.
        *) return "${CHECK_MISSING}" ;;
    esac
}

# run_step_forward <id> <file>
# Reports check()'s verdict before doing anything, then applies if needed.
run_step_forward() {
    local id="$1" file="$2" start elapsed
    GENTIAN_CURRENT_STEP="$id"

    _load_step "$file"
    _step_banner "$id" "$file"

    if ! _has_verb apply; then
        error "Step ${id} defines no apply()"
        return 1
    fi

    if _has_verb check; then
        local verdict=0
        _step_verdict || verdict=$?
        case "$verdict" in
            "${CHECK_SATISFIED}")
                echo "     check: satisfied  →  skip"
                return 0 ;;
            "${CHECK_UNDEFINED}")
                # Not applicable to this cluster — the feature is off, or there
                # is no install-time artefact. Skipped for the same reason a
                # satisfied step is, and named differently so the log does not
                # claim it verified something.
                echo "     check: undefined (nothing to do here)  →  skip"
                return 0 ;;
            "${CHECK_ALWAYS}")
                echo "     check: runs every pass  →  applying" ;;
            *)
                echo "     check: not satisfied  →  applying" ;;
        esac
    else
        # A step with no check() cannot be skipped and cannot be dry-run
        # honestly. Steps that only wait on a condition are the legitimate case.
        echo "     check: none  →  applying"
    fi

    if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
        echo "     (dry run — not applying)"
        return 0
    fi

    start=$SECONDS
    # Explicitly, rather than letting `set -e` end the run.
    #
    # The ERR trap stays deliberately silent when the failing command is a
    # `return N`, because a function that returns has usually printed its own
    # diagnosis. But `warn` then `return 1` prints something that reads as
    # benign and stops the install anyway: D-05 ended on a WARN and a shell
    # prompt, which is indistinguishable from a completed run. The driver knows
    # which step failed, so the driver says so.
    if ! apply; then
        local rc=$?
        elapsed=$((SECONDS - start))
        error "Step ${id} failed after ${elapsed}s (apply() returned ${rc})."
        error "  Nothing after it has run. Fix the cause and re-run — steps that"
        error "  already succeeded report satisfied and are skipped."
        return "${rc}"
    fi
    elapsed=$((SECONDS - start))
    echo -e "     ${GREEN}✓${NC} ${elapsed}s"
}

# run_step_reverse <id> <file> — teardown. Missing destroy() is not an error:
# steps whose resources are owned by ArgoCD or Crossplane have nothing of their
# own to remove, and saying so is more useful than an empty function.
run_step_reverse() {
    local id="$1" file="$2" start elapsed
    GENTIAN_CURRENT_STEP="$id"

    _load_step "$file"
    _step_banner "$id" "$file"

    if ! _has_verb destroy; then
        echo "     destroy: not defined  →  skip"
        return 0
    fi

    # check() does not gate destroy(), in any direction.
    #
    # A check answers "is this step's work complete", which on the reverse pass
    # is the wrong question: what matters is whether anything is left. The two
    # differ for every partially-torn-down step, and the reverse pass creates
    # that state deliberately — each step removes part of what the steps before
    # it tested for.
    #
    # A-03-namespaces is the case that proves it. Its check ANDs eight
    # namespaces, two of which (external-secrets, argocd) A-08 and A-09 have
    # already deleted by the time the reverse pass arrives. It therefore always
    # reports MISSING, its destroy() was always skipped, and six kernel
    # namespaces survived an uninstall that reported success. A-02 lost 422
    # provider CRDs the same way.
    #
    # Running it regardless is free: every destroy() here is built from
    # --ignore-not-found deletes, which is what made skipping look like a safe
    # optimisation in the first place.
    if _has_verb check; then
        local verdict=0
        _step_verdict || verdict=$?
        if [[ "$verdict" == "${CHECK_MISSING}" ]]; then
            echo "     check: reports absent  →  destroying anyway"
        fi
    fi

    if [[ "${GENTIAN_DRY_RUN}" == "1" ]]; then
        echo "     (dry run — not destroying)"
        return 0
    fi

    start=$SECONDS
    # Teardown is best-effort by design: a half-installed cluster must still
    # uninstall to completion rather than stopping at the first absent object.
    destroy || warn "destroy() for ${id} reported errors; continuing teardown."
    elapsed=$((SECONDS - start))
    echo -e "     ${GREEN}✓${NC} ${elapsed}s"
}

# =============================================================================
# Drive
# =============================================================================

# drive_forward — install / update.
drive_forward() {
    discover_steps || return 1
    local i id file ran=0
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"
        step_selected "$id" || continue
        run_step_forward "$id" "$file"
        ran=$((ran + 1))
    done
    echo ""
    if [[ $ran -eq 0 ]]; then
        warn "No steps matched the current selection."
    fi
    # No step is current once the pass is over. Anything that fails afterwards —
    # a purge helper, a summary — was reported against whichever step happened to
    # run last, which named A-01-crossplane for a fault in purge_report_cluster_residue.
    GENTIAN_CURRENT_STEP=""
    return 0
}

# drive_reverse — uninstall. Iterates the same list backwards, so teardown
# order is a property of the step numbering rather than a second hand-kept list.
drive_reverse() {
    discover_steps || return 1
    local i id file
    for (( i = ${#_STEP_IDS[@]} - 1; i >= 0; i-- )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"
        step_selected "$id" || continue
        run_step_reverse "$id" "$file"
    done
    echo ""
    # No step is current once the pass is over. Anything that fails afterwards —
    # a purge helper, a summary — was reported against whichever step happened to
    # run last, which named A-01-crossplane for a fault in purge_report_cluster_residue.
    GENTIAN_CURRENT_STEP=""
    return 0
}

# drive_explain — the contract table. No cluster access, so this works with no
# kubeconfig and answers "what will this do to my cluster" before anything runs.
drive_explain() {
    discover_steps || return 1
    local i id file
    local last=""
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"
        step_selected "$id" || continue
        local ph; ph="$(step_header "$file" phase)"
        if [[ "${ph}" != "${last}" ]]; then
            last="${ph}"
            printf '\n%b%s%b\n' "${CYAN}" "${ph}" "${NC}"
        fi
        printf '  %-4s %-26s %s\n' \
            "$(step_number_of "$id")" "$(step_label_of "$id")" "$(step_header "$file" provides)"
    done
    echo ""
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"
        step_selected "$id" || continue
        local mutates
        mutates="$(step_header "$file" mutates)"
        [[ -n "$mutates" ]] && printf '  %-26s mutates: %s\n' "$(step_label_of "$id")" "$mutates"
    done
    echo ""
    return 0
}

# drive_status — run every check() and report, mutating nothing. The read-only
# answer to "where did this install get to".
drive_status() {
    # Before any check runs: the same configuration the install would resolve,
    # or every stage-scoped and cluster-scoped check answers about a different
    # cluster than the one in front of it.
    load_status_context
    discover_steps || return 1
    local i id file state verdict
    echo ""
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"
        step_selected "$id" || continue
        _load_step "$file"
        verdict=0
        _step_verdict || verdict=$?
        case "$verdict" in
            # Green is a claim about the cluster: this was asked, and verified.
            "${CHECK_SATISFIED}") state="${GREEN}satisfied${NC}" ;;
            "${CHECK_UNDEFINED}") state="${YELLOW}undefined${NC}" ;;
            # Not a fault: the step reports no state because it has none to
            # report, and red would say something is wrong with the cluster.
            "${CHECK_ALWAYS}")    state="${YELLOW}always${NC}" ;;
            *)                    state="${RED}missing${NC}" ;;
        esac
        printf '  [%s] %-26s %b\n' "$(step_number_of "$id")" "$(step_label_of "$id")" "$state"
    done
    echo ""
    return 0
}

# =============================================================================
# Contract validation — used by CI and by `--validate`
# =============================================================================

# validate_steps — every step must declare `step:` and `provides:` and define
# apply(). Enforced in CI so a new step cannot skip the contract.
validate_steps() {
    discover_steps || return 1
    local i id file rc=0 declared
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        id="${_STEP_IDS[$i]}"
        file="${_STEP_FILES[$i]}"

        declared="$(step_header "$file" step)"
        if [[ "$declared" != "$id" ]]; then
            error "${id}: header 'step: ${declared}' does not match filename"
            rc=1
        fi
        if [[ -z "$(step_header "$file" provides)" ]]; then
            error "${id}: missing 'provides:' header"
            rc=1
        fi
        if [[ -z "$(step_header "$file" phase)" ]]; then
            error "${id}: missing 'phase:' header"
            rc=1
        fi
        _load_step "$file"
        if ! _has_verb apply; then
            error "${id}: no apply() defined"
            rc=1
        fi
        if ! _has_verb check; then
            warn "${id}: no check() — cannot be skipped on re-run or dry-run"
        fi
    done

    validate_pins || rc=1
    validate_step_calls || rc=1

    [[ $rc -eq 0 ]] && success "Step contracts valid (${#_STEP_IDS[@]} steps)."
    return $rc
}

# _step_shell_lines <file> — the file with heredoc bodies removed, so only
# actual shell is scanned. Steps legitimately embed YAML this way.
_step_shell_lines() {
    awk '
        # Inside a heredoc: emit nothing until the terminator.
        inhd {
            line = $0
            sub(/^[ \t]+/, "", line)
            if (line == delim) inhd = 0
            next
        }
        # Opening delimiter, quoted or not, with or without the <<- dash form.
        match($0, /<<-?[ ]*[\047"]?[A-Za-z_][A-Za-z0-9_]*[\047"]?/) {
            d = substr($0, RSTART, RLENGTH)
            gsub(/^<<-?[ ]*|[\047"]/, "", d)
            delim = d
            inhd = 1
        }
        { print }
    ' "$1"
}

# validate_step_calls — every library function a step calls must be defined.
#
# This exists because it was NOT caught once: when install.sh became a driver
# and uninstall.sh was deleted, the bodies those steps called went with them.
# Every contract still validated and shellcheck stayed clean, because nothing
# checked that a step's apply() could actually resolve what it invokes. The
# break would only have surfaced as a "command not found" partway through a real
# install, against a real cluster.
#
# Functions a step pulls in by sourcing a file inside apply() are resolved by
# sourcing the same files here first.
validate_step_calls() {
    local file fn rc=0 src

    # Anything a step sources at apply() time, loaded up front so its functions
    # are visible to the check.
    # shellcheck disable=SC2016  # the pattern matches a literal ${SCRIPT_DIR}
    while IFS= read -r src; do
        [[ -n "${src}" && -f "${SCRIPT_DIR}/${src}" ]] || continue
        # shellcheck source=/dev/null
        source "${SCRIPT_DIR}/${src}" >/dev/null 2>&1 || true
    done < <(grep -ho 'source "\${SCRIPT_DIR}/[^"]*"' "${GENTIAN_STEPS_DIR}"/*.sh 2>/dev/null |
             sed 's|source "\${SCRIPT_DIR}/||; s|"$||' | sort -u)

    # install.sh's own prepare_run is checked too. Leaving it out is how
    # scaffold_cluster_deployment survived as a call to a function that no
    # longer existed: every step validated, and the one place the driver calls
    # library code directly was the one place nothing looked.
    local self_defined
    self_defined=" $(grep -oE '^[a-z_][a-z0-9_]*\(\)' "${SCRIPT_DIR}/install.sh" 2>/dev/null |
                     tr -d '()' | tr '\n' ' ')"

    for file in "${SCRIPT_DIR}/install.sh" "${_STEP_FILES[@]}"; do
        # Calls at the top of a verb body: four-space indent, bare command.
        while IFS= read -r fn; do
            [[ -n "${fn}" ]] || continue
            # Shell keywords, not calls. 'esac' is quoted because an unquoted
            # one inside a pattern list ends the case statement instead of
            # matching — the pattern silently stops covering everything after it.
            case "${fn}" in
                local|return|echo|export|source|if|fi|then|else|elif|for|while|do|done|case|'esac'|break|continue|exit)
                    continue ;;
            esac
            # A real external command is fine; only unresolvable names matter.
            command -v "${fn}" >/dev/null 2>&1 && continue
            declare -F "${fn}" >/dev/null 2>&1 && continue
            # Defined in install.sh itself, so it resolves at run time there.
            [[ "${self_defined}" == *" ${fn} "* ]] && continue
            error "$(step_id_of "${file}"): calls '${fn}', which is not defined or on PATH"
            rc=1
            # Heredoc bodies are stripped first: a step that applies YAML inline
            # has `    phase: bootstrap` at the same indent as a call, and the
            # lint would read "phase" as an undefined function. The trailing '='
            # is captured so assignments drop out too — `    claim="$(...)"` is a
            # local variable, not a call.
        done < <(_step_shell_lines "${file}" |
                 grep -oE '^ {4}[a-z_][a-z0-9_]*=?' | grep -v '=$' | tr -d ' ' | sort -u)
    done
    return $rc
}

# validate_pins — every `# pins:` names a component in versions.yaml, and every
# component in versions.yaml is claimed by a step.
#
# The second half is the one that matters: versions.yaml is the mirror inventory
# for an air-gapped install, and an unclaimed entry means either a component the
# installer no longer pulls (so a mirror wastes effort on it) or, worse, a pin
# that silently stopped being read while the installer went on pulling something
# unpinned.
validate_pins() {
    local i file rc=0 c claimed=" " comp
    for (( i = 0; i < ${#_STEP_IDS[@]}; i++ )); do
        file="${_STEP_FILES[$i]}"
        c="$(step_header "$file" pins)"
        [[ -n "$c" ]] || continue
        if ! _gentian_yq ".\"${c}\"" "${GENTIAN_VERSIONS_FILE}" >/dev/null 2>&1; then
            error "$(step_id_of "$file"): pins '${c}', which is not in versions.yaml"
            rc=1
        fi
        claimed+="${c} "
    done

    while IFS= read -r comp; do
        [[ -n "$comp" ]] || continue
        if [[ "${claimed}" != *" ${comp} "* ]]; then
            error "versions.yaml pins '${comp}', which no step claims with '# pins:'"
            rc=1
        fi
    done < <(gentian_pin_components)

    return $rc
}
