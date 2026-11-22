/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func (r *TenantReconciler) ensureGentianGroupsJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, gentianGroupsJobName(tenant.Name))
}

// makeGentianGroupsJob is the Job that settles what every token of the realm
// says of a person beyond what the realm's defaults do: today, that the
// email claim follows the username. It keeps the name it had when it also
// made the tenant's groups, because the Composition holds it by that name.
func makeGentianGroupsJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	backoff := meta.ProvisioningJobBackoffLimit
	container := keycloakContainer("provision-gentian-groups", buildGentianGroupsScript(realmName))
	job := newKeycloakJob(gentianGroupsJobName(tenant.Name), tenant, "", container)
	job.Spec.BackoffLimit = &backoff
	return job
}

// buildGentianGroupsScript repoints the realm's email claim at the username.
// It makes no group and no groups scope: who holds what is in the rights
// store, and no token names a group.
func buildGentianGroupsScript(realmName string) string {
	return keycloak.ShellJSONIDExtractor() + keycloak.ShellAdminCall() + fmt.Sprintf(`set -eu
REALM=%q
`+keycloak.ShellAdminToken()+`
AUTH_HEADER="Authorization: Bearer ${TOKEN}"
%s

# ── The email claim must follow the USERNAME, not the email field ────────────
#
# Keycloak's own password reset mails whatever is in the user's email field, and
# no setting points it at an attribute instead. So the field holds the address a
# locked-out human can still read — their recovery address — which means it can no
# longer be the value applications read as the user's identity.
#
# The username already IS the workspace address (christian@corp.example is both
# the login and the mailbox), so repointing the built-in email mapper at it keeps
# every token and userinfo response exactly as it was. For every user that exists
# when this first runs the two are the same string, so the claim does not change
# at all; afterwards it stays the workspace address while the field becomes the
# recovery address.
#
# Every request's status is checked: a request that fails must never be
# indistinguishable from an answer. A 401 once came back as "the email scope
# has no mapper named email" -- a Keycloak state that was not the case,
# reported as though it had been read.
#
# The mapper is patched field-by-field through jq rather than rebuilt, because a
# PUT replaces the config wholesale: naming only what changes leaves Keycloak's
# own keys (id.token.claim, jsonType.label …) exactly as it wrote them, so
# nothing here and nothing in the Composition sees drift.
EMAIL_SCOPES_FILE=$(mktemp)
EMAIL_SCOPES_CODE=$(curl -s -o "${EMAIL_SCOPES_FILE}" -w '%%{http_code}' -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes")
if [ "${EMAIL_SCOPES_CODE}" != "200" ]; then
  echo "ERROR: listing client scopes returned HTTP ${EMAIL_SCOPES_CODE}" >&2
  rm -f "${EMAIL_SCOPES_FILE}"
  exit 1
fi
keycloak_json_id_by_attr "$(cat "${EMAIL_SCOPES_FILE}")" "name" "email"
rm -f "${EMAIL_SCOPES_FILE}"
EMAIL_SCOPE_ID="${_kj_id}"
if [ -z "${EMAIL_SCOPE_ID}" ]; then
  echo "ERROR: realm ${REALM} has no email client scope, so the email claim cannot be separated from the reset address" >&2
  exit 1
fi
EMAIL_MAPPERS_FILE=$(mktemp)
EMAIL_MAPPERS_CODE=$(curl -s -o "${EMAIL_MAPPERS_FILE}" -w '%%{http_code}' -H "${AUTH_HEADER}" \
  "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${EMAIL_SCOPE_ID}/protocol-mappers/models")
if [ "${EMAIL_MAPPERS_CODE}" != "200" ]; then
  echo "ERROR: listing the email scope's mappers returned HTTP ${EMAIL_MAPPERS_CODE}" >&2
  rm -f "${EMAIL_MAPPERS_FILE}"
  exit 1
fi
EMAIL_MAPPER=$(jq -c '[.[] | select(.name == "email")][0] // empty' < "${EMAIL_MAPPERS_FILE}")
rm -f "${EMAIL_MAPPERS_FILE}"
if [ -z "${EMAIL_MAPPER}" ]; then
  echo "ERROR: the email client scope in realm ${REALM} has no mapper named email" >&2
  exit 1
fi
EMAIL_MAPPER_ID=$(printf '%%s' "${EMAIL_MAPPER}" | jq -r '.id')
EMAIL_MAPPER_SOURCE=$(printf '%%s' "${EMAIL_MAPPER}" | jq -r '.config["user.attribute"] // ""')
if [ "${EMAIL_MAPPER_SOURCE}" = "username" ]; then
  echo "email claim already follows the username"
else
  UPDATED_MAPPER=$(printf '%%s' "${EMAIL_MAPPER}" | jq -c '.config["user.attribute"] = "username"')
  EMAIL_PUT_CODE=$(curl -s -o /dev/null -w '%%{http_code}' -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/client-scopes/${EMAIL_SCOPE_ID}/protocol-mappers/models/${EMAIL_MAPPER_ID}" \
    -d "${UPDATED_MAPPER}")
  case "${EMAIL_PUT_CODE}" in
  20*) echo "email claim repointed from ${EMAIL_MAPPER_SOURCE:-<unset>} to the username" ;;
  *)
    echo "ERROR: repointing the email claim returned HTTP ${EMAIL_PUT_CODE}" >&2
    exit 1
    ;;
  esac
fi

`, realmName, keycloak.ShellWaitForRealm("${REALM}"))
}
