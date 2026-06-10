// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

// ensureLDAPUIDAttributeMapperShell defines a shell helper that imports the LDAP
// uid attribute into Keycloak user attribute uid. Portal login uses
// mailPrimaryAddress as username in the kernel realm; OIDC packs map
// user.attribute uid → opendesk_username, so the uid attribute must exist.
const ensureLDAPUIDAttributeMapperShell = `
ensure_ldap_uid_attribute_mapper() {
  local REALM="$1"
  local LDAP_NAME="$2"
  local AUTH_HEADER="Authorization: Bearer ${TOKEN}"

  local LDAP_COMPONENTS
  LDAP_COMPONENTS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" || echo "[]")
  keycloak_json_id_by_attr "${LDAP_COMPONENTS}" "name" "${LDAP_NAME}"
  local LDAP_ID="${_kj_id}"
  if [ -z "${LDAP_ID}" ]; then
    echo "ldap provider ${LDAP_NAME} not found in realm ${REALM}; skip uid mapper"
    return 0
  fi

  local MAPPERS
  MAPPERS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?parent=${LDAP_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper" || echo "[]")
  if echo "${MAPPERS}" | grep -Fq '"name":"uid"'; then
    echo "uid ldap attribute mapper already on ${LDAP_NAME} in realm ${REALM}"
    return 0
  fi

  curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components" \
    -d "{\"name\":\"uid\",\"providerId\":\"user-attribute-ldap-mapper\",\"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",\"parentId\":\"${LDAP_ID}\",\"config\":{\"ldap.attribute\":[\"uid\"],\"user.model.attribute\":[\"uid\"],\"read.only\":[\"true\"],\"always.read.value.from.ldap\":[\"true\"]}}"
  echo "uid ldap attribute mapper added on ${LDAP_NAME} in realm ${REALM}"

  curl -sf --max-time 120 -X POST -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${LDAP_ID}/sync?action=triggerFullSync" >/dev/null 2>&1 || true
}
`