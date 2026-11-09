#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-mail-listeners.sh
# =============================================================================
# Which ports the mail edge's load balancer serves, rendered with helm and no
# cluster: the mail ApplicationSet as the wrapper writes it, then the edge's
# chart with the parameters Argo CD passes it (scripts/tests/mail_listeners.py).
# =============================================================================
# Every check is a string handed to eval, so what it names is expanded there.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

echo ""
echo "mail edge: the ports its load balancer serves"
echo ""

ports() { python3 scripts/tests/mail_listeners.py ports "$@" 2>&1; }

out="$(ports --set mailServiceMode=system)"
check "a cluster that runs its own mail serves 25, 587 and 993" '[[ "${out}" == "25 587 993" ]]' "${out}"

finish
