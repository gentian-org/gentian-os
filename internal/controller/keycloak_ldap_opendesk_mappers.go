// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

// ensureLDAPOpenDeskAttributeMappersShell defines shell helpers that import
// OpenDesk LDAP attributes into Keycloak user profiles. OIDC pack protocol
// mappers read these attributes when building ID tokens (e.g. ox_context maps
// user.attribute oxContextIDNum → claim context for OX App Suite login).
const ensureLDAPOpenDeskAttributeMappersShell = `
ensure_ldap_oxcontext_attribute_mapper() {
  local REALM="$1"
  local LDAP_NAME="$2"
  local AUTH_HEADER="Authorization: Bearer ${TOKEN}"

  local LDAP_COMPONENTS
  LDAP_COMPONENTS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" || echo "[]")
  keycloak_json_id_by_attr "${LDAP_COMPONENTS}" "name" "${LDAP_NAME}"
  local LDAP_ID="${_kj_id}"
  if [ -z "${LDAP_ID}" ]; then
    echo "ldap provider ${LDAP_NAME} not found in realm ${REALM}; skip oxContextIDNum mapper"
    return 0
  fi

  local MAPPERS
  MAPPERS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?parent=${LDAP_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper" || echo "[]")
  if echo "${MAPPERS}" | grep -Fq '"name":"oxContextIDNum"'; then
    echo "oxContextIDNum ldap attribute mapper already on ${LDAP_NAME} in realm ${REALM}"
    return 0
  fi

  curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components" \
    -d "{\"name\":\"oxContextIDNum\",\"providerId\":\"user-attribute-ldap-mapper\",\"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",\"parentId\":\"${LDAP_ID}\",\"config\":{\"ldap.attribute\":[\"oxContextIDNum\"],\"user.model.attribute\":[\"oxContextIDNum\"],\"read.only\":[\"true\"],\"always.read.value.from.ldap\":[\"true\"],\"is.mandatory.in.ldap\":[\"false\"]}}"
  echo "oxContextIDNum ldap attribute mapper added on ${LDAP_NAME} in realm ${REALM}"

  curl -sf --max-time 120 -X POST -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${LDAP_ID}/sync?action=triggerFullSync" >/dev/null 2>&1 || true
}

ensure_ldap_entryuuid_attribute_mapper() {
  local REALM="$1"
  local LDAP_NAME="$2"
  local AUTH_HEADER="Authorization: Bearer ${TOKEN}"

  local LDAP_COMPONENTS
  LDAP_COMPONENTS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" || echo "[]")
  keycloak_json_id_by_attr "${LDAP_COMPONENTS}" "name" "${LDAP_NAME}"
  local LDAP_ID="${_kj_id}"
  if [ -z "${LDAP_ID}" ]; then
    echo "ldap provider ${LDAP_NAME} not found in realm ${REALM}; skip entryUUID mapper"
    return 0
  fi

  local MAPPERS
  MAPPERS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?parent=${LDAP_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper" || echo "[]")
  if echo "${MAPPERS}" | grep -Fq '"name":"entryUUID"'; then
    echo "entryUUID ldap attribute mapper already on ${LDAP_NAME} in realm ${REALM}"
    return 0
  fi

  curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components" \
    -d "{\"name\":\"entryUUID\",\"providerId\":\"user-attribute-ldap-mapper\",\"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",\"parentId\":\"${LDAP_ID}\",\"config\":{\"ldap.attribute\":[\"entryUUID\"],\"user.model.attribute\":[\"entryUUID\"],\"read.only\":[\"true\"],\"always.read.value.from.ldap\":[\"true\"],\"is.mandatory.in.ldap\":[\"false\"]}}"
  echo "entryUUID ldap attribute mapper added on ${LDAP_NAME} in realm ${REALM}"

  curl -sf --max-time 120 -X POST -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${LDAP_ID}/sync?action=triggerFullSync" >/dev/null 2>&1 || true
}
`
