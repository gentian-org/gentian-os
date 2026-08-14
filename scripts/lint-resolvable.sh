#!/usr/bin/env bash
# =============================================================================
# scripts/lint-resolvable.sh — every function call must resolve
# =============================================================================
# Deleting a function whose last caller you did not check is the single most
# repeated mistake in this repository's recent history. It has happened three
# times: when install.sh became a driver and took thirteen step bodies with it,
# when scaffold_cluster_deployment was left called-but-undefined, and when
# save_install_state was removed with six live callers.
#
# Every one of them passed shellcheck, passed the step-contract check, and would
# have failed partway through a real install with "command not found".
#
# This walks every shell file on the install path and asserts that each function
# it calls is defined somewhere reachable — in the library, in the file itself,
# or on PATH.
#
# Usage:
#   scripts/lint-resolvable.sh
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
SCRIPT_DIR="$PWD"
export SCRIPT_DIR

# shellcheck source=scripts/lib/load.sh
source scripts/lib/load.sh >/dev/null 2>&1
# shellcheck source=scripts/lib/driver.sh
source scripts/lib/driver.sh >/dev/null 2>&1
# Pulled in by steps at apply() time rather than by load.sh.
# shellcheck source=scripts/portal-login-bootstrap.sh
source scripts/portal-login-bootstrap.sh >/dev/null 2>&1 || true

# load.sh installs an ERR trap for install runs. This is a lint: a grep that
# matches nothing is a normal outcome, not an aborted install.
trap - ERR
set +e

# Set after load.sh, which defines its own colour vars for `echo -e` and would
# otherwise overwrite these with strings printf renders literally.
RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; NC=$'\033[0m'

# Names defined in a file itself resolve at that file's run time even if the
# library never sees them — install.sh's own helpers, for instance.
_self_defined() {
    grep -hoE '^[a-z_][a-z0-9_]*\(\)' "$1" 2>/dev/null | tr -d '()' | tr '\n' ' '
}

# Heredoc bodies are data, not shell: a step applying inline YAML has
# `    phase: bootstrap` at call indent and would read as a call.
_shell_only() {
    awk '
        inhd { line=$0; sub(/^[ \t]+/,"",line); if (line==delim) inhd=0; next }
        match($0, /<<-?[ ]*[\047"]?[A-Za-z_][A-Za-z0-9_]*[\047"]?/) {
            d=substr($0,RSTART,RLENGTH); gsub(/^<<-?[ ]*|[\047"]/,"",d); delim=d; inhd=1
        }
        { print }
    ' "$1"
}

files=$(git ls-files '*.sh' scripts/kubectl-gentian 2>/dev/null | grep -vE 'lint-resolvable\.sh$')
fail=0
checked=0

for f in ${files}; do
    self=" $(_self_defined "${f}")"
    while IFS= read -r fn; do
        [[ -n "${fn}" ]] || continue
        case "${fn}" in
            local|return|echo|export|source|if|fi|then|else|elif|for|while|do|done|case|'esac'|\
            break|continue|exit|shift|read|eval|set|unset|trap|declare|readonly|printf|cd|wait|exec)
                continue ;;
            # awk builtins, from the awk programs embedded in kubectl-gentian.
            next|print|getline|delete)
                continue ;;
            # Step verbs. Defined by whichever step the driver has sourced, so
            # they never resolve statically and that is correct.
            apply|check|destroy)
                continue ;;
        esac
        command -v "${fn}" >/dev/null 2>&1 && continue
        declare -F "${fn}" >/dev/null 2>&1 && continue
        [[ "${self}" == *" ${fn} "* ]] && continue
        printf '  %s✗%s %-34s calls %s, which is not defined or on PATH\n' \
            "${RED}" "${NC}" "${f#scripts/}" "${fn}"
        fail=1
    done < <(_shell_only "${f}" | grep -oE '^[ ]{0,8}[a-z_][a-z0-9_]*=?[ ]*$' |
             tr -d ' ' | grep -v '=$' | sort -u)
    checked=$((checked + 1))
done

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}Every function call resolves${NC} (${checked} files)."
    exit 0
fi
echo "${RED}Unresolvable call(s).${NC} A function was deleted with callers still live,"
echo "or a call was renamed in one place only."
exit 1
