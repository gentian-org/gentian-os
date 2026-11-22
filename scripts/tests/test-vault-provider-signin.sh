#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-vault-provider-signin.sh
# =============================================================================
# Crossplane's provider-vault holds no vault token. It signs in at the vault's
# Kubernetes auth as its own ServiceAccount, and that takes four places saying
# the same names:
#
#   scripts/lib/bootstrap.sh              makes the account and writes the role
#   crossplane/providers/providers.yaml   runs the provider as the account
#   crossplane/providers/provider-configs.yaml   signs in with the role
#   crossplane/compositions/cluster-default.yaml keeps the role
#
# A name that differs in one of them is a provider the vault turns away, and
# with it every role and policy of the Cluster composition. Held here, with:
#
#   - vault_provider_signs_in answers from the vault's own answer, and no
#     without the account;
#   - B-04's check reads the cluster only: the account, and a role the
#     provider keeps in sync;
#   - where the account already signs in, the bootstrap asks for no vault
#     token, which is every run on a cluster whose root token is gone.
#
# kubectl and bao_curl are stand-ins; no cluster, no vault.
# =============================================================================

# shellcheck disable=SC2016 # the checks are strings the harness evaluates
# shellcheck disable=SC2034,SC2317,SC2329 # read and called by the code under test
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

LIB="scripts/lib/bootstrap.sh"
PROVIDERS="crossplane/providers/providers.yaml"
CONFIGS="crossplane/providers/provider-configs.yaml"
COMPOSITION="crossplane/compositions/cluster-default.yaml"

eval "$(grep -E '^VAULT_PROVIDER_(SA|ROLE|AUTH_MOUNT)=' "${LIB}")"

echo ""
echo "the names agree"
echo ""

check "the library names an account, a role and a mount" \
    '[[ -n "${VAULT_PROVIDER_SA:-}" && -n "${VAULT_PROVIDER_ROLE:-}" && -n "${VAULT_PROVIDER_AUTH_MOUNT:-}" ]]'

# The vault-config runtime config, from its name to the end of its document.
vault_config="$(awk '/^  name: vault-config$/ { on = 1 } on && /^---/ { exit } on' "${PROVIDERS}")"
check "the provider runs as that account" \
    'grep -qE "^          serviceAccountName: ${VAULT_PROVIDER_SA}\$" <<<"${vault_config}"' "${vault_config}"

# The vault ProviderConfig, from its apiVersion to the end of its document.
vault_pc="$(awk '/^apiVersion: vault\.upbound\.io\// { on = 1 } on && /^---/ { exit } on' "${CONFIGS}")"
check "the ProviderConfig signs in at the Kubernetes auth" \
    'grep -qE "^    source: Kubernetes\$" <<<"${vault_pc}"' "${vault_pc}"
check "the ProviderConfig names that role" \
    'grep -qE "^  role: ${VAULT_PROVIDER_ROLE}\$" <<<"${vault_pc}"' "${vault_pc}"
check "the ProviderConfig reads no Secret" \
    '! grep -q "secretRef" <<<"${vault_pc}"' "${vault_pc}"

# The composed role, from its resource name to its providerConfigRef.
composed="$(awk '/composition-resource-name: vault-auth-role-crossplane$/ { on = 1 } on { print } on && /providerConfigRef:/ { exit }' "${COMPOSITION}")"
check "the composition keeps the role under that name" \
    'grep -qE "^ +roleName: ${VAULT_PROVIDER_ROLE}\$" <<<"${composed}"' "${composed}"
check "the composed role binds that account" \
    'grep -qE "^ +- ${VAULT_PROVIDER_SA}\$" <<<"${composed}"' "${composed}"
check "the composed role carries crossplane-write" \
    'grep -qE "^ +- crossplane-write\$" <<<"${composed}"' "${composed}"

# The provider signs in at the mount named "kubernetes" whatever the claim
# says (provider-vault's Kubernetes source knows no other), so the library
# must write the role there.
check "the role is written at the mount the provider signs in at" \
    '[[ "${VAULT_PROVIDER_AUTH_MOUNT}" == "kubernetes" ]]'

check "B-04's destroy removes that account" \
    'grep -q "delete serviceaccount ${VAULT_PROVIDER_SA} " scripts/steps/B-04-vault-auth.sh'
check "no step mints a token for the provider" \
    '! grep -rn "token create" scripts/lib/bootstrap.sh scripts/steps/B-04-vault-auth.sh'

echo ""
echo "vault_provider_signs_in"
echo ""

info() { :; }
warn() { :; }
error() { printf '%s\n' "$*" >>"${WORK}/errors"; }
success() { :; }
banner() { :; }
ns_kernel() { echo "kernel-$1"; }
gentian_service_addr() { echo "https://openbao.test:8200"; }
openbao_cli_trust() { :; }
_kubectl_retry() { cat >/dev/null; echo "apply" >>"${WORK}/calls"; }

# kubectl: the account exists when HAS_SA=1, and a token is issued for it;
# the provider's roles report SYNCED, one word per role.
kubectl() {
    printf '%s\n' "$1 $2" >>"${WORK}/kubectl-calls"
    case "$1 $2" in
        "get serviceaccount") [[ "${HAS_SA}" == "1" ]] ;;
        "get authbackendroles.kubernetes.vault.upbound.io") printf '%s' "${SYNCED:-}" ;;
        "create token")       echo "a.jwt.token" ;;
        *) return 1 ;;
    esac
}
# The vault's answer to the sign-in is STUB_CODE; what was asked is kept.
bao_curl() {
    printf '%s\n' "$*" >>"${WORK}/vault-calls"
    printf '%s' "${STUB_CODE}"
}
_resolve_bao_token() { echo "asked" >>"${WORK}/token-asked"; return 1; }

eval "$(sed -n '/^vault_provider_in_sync() {/,/^}/p; /^vault_provider_signs_in() {/,/^}/p; /^bootstrap_openbao_for_crossplane() {/,/^}/p' "${LIB}")"

HAS_SA=0 STUB_CODE=200
check "no account: not signed in, and the vault is not asked" \
    '! vault_provider_signs_in && [[ ! -e "${WORK}/vault-calls" ]]'

HAS_SA=1 STUB_CODE=200
check "the vault says 200: signed in" 'vault_provider_signs_in'
check "asked at the Kubernetes auth's login" \
    'grep -q "/v1/auth/kubernetes/login" "${WORK}/vault-calls"' "$(cat "${WORK}/vault-calls" 2>/dev/null)"
check "the account's token is not on the command line" \
    '! grep -q "a.jwt.token" "${WORK}/vault-calls"' "$(cat "${WORK}/vault-calls" 2>/dev/null)"

HAS_SA=1 STUB_CODE=400
check "the vault says 400 (no such role): not signed in" '! vault_provider_signs_in'
HAS_SA=1 STUB_CODE=403
check "the vault says 403 (another account): not signed in" '! vault_provider_signs_in'

echo ""
echo "vault_provider_in_sync, which is B-04's check"
echo ""

: >"${WORK}/kubectl-calls"
HAS_SA=0 SYNCED="True True"
check "no account: no" '! vault_provider_in_sync'
HAS_SA=1 SYNCED=""
check "no role of the provider's yet: no" '! vault_provider_in_sync'
HAS_SA=1 SYNCED="False False False"
check "every role out of sync: no" '! vault_provider_in_sync'
HAS_SA=1 SYNCED="False True False"
check "a role in sync: yes" 'vault_provider_in_sync'
check "it only reads" '! grep -qv "^get " "${WORK}/kubectl-calls"' "$(cat "${WORK}/kubectl-calls")"
check "B-04's check is that and nothing else" \
    '[[ "$(sed -n "/^check() {/,/^}/p" scripts/steps/B-04-vault-auth.sh | sed "1d;\$d" | xargs)" == "vault_provider_in_sync" ]]'

echo ""
echo "the bootstrap where the account already signs in"
echo ""

HAS_SA=1 STUB_CODE=200
rc=0; (bootstrap_openbao_for_crossplane) || rc=$?
check "it succeeds" '[[ "${rc}" == "0" ]]'
check "it makes sure of the account" 'grep -q apply "${WORK}/calls"'
check "it asks for no vault token" '[[ ! -e "${WORK}/token-asked" ]]'

HAS_SA=1 STUB_CODE=400
rc=0; (bootstrap_openbao_for_crossplane) >/dev/null 2>&1 || rc=$?
check "where the vault refuses the account and no token is at hand, it stops" \
    '[[ "${rc}" != "0" ]] && [[ -e "${WORK}/token-asked" ]]'

finish
