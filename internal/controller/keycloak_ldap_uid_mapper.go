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

// ensureLDAPEmailAttributeMapperShell maps Univention mailPrimaryAddress into the
// Keycloak email field on tenant LDAP federation. Broker idp-auto-link matches
// federated users by email; Keycloak's default email mapper uses LDAP mail, which
// openDesk tenants do not populate.
const ensureLDAPEmailAttributeMapperShell = `
ensure_ldap_email_attribute_mapper() {
  local REALM="$1"
  local LDAP_NAME="$2"
  local AUTH_HEADER="Authorization: Bearer ${TOKEN}"

  local LDAP_COMPONENTS
  LDAP_COMPONENTS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?type=org.keycloak.storage.UserStorageProvider" || echo "[]")
  keycloak_json_id_by_attr "${LDAP_COMPONENTS}" "name" "${LDAP_NAME}"
  local LDAP_ID="${_kj_id}"
  if [ -z "${LDAP_ID}" ]; then
    echo "ldap provider ${LDAP_NAME} not found in realm ${REALM}; skip email mapper"
    return 0
  fi

  local MAPPERS
  MAPPERS=$(curl -sf --max-time 30 -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/components?parent=${LDAP_ID}&type=org.keycloak.storage.ldap.mappers.LDAPStorageMapper" || echo "[]")
  keycloak_json_id_by_attr "${MAPPERS}" "name" "email"
  local EMAIL_ID="${_kj_id}"
  local EMAIL_CONFIG="{\"ldap.attribute\":[\"mailPrimaryAddress\"],\"user.model.attribute\":[\"email\"],\"read.only\":[\"true\"],\"always.read.value.from.ldap\":[\"true\"],\"is.mandatory.in.ldap\":[\"false\"]}"

  if [ -n "${EMAIL_ID}" ]; then
    curl -sf --max-time 30 -X PUT -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/components/${EMAIL_ID}" \
      -d "{\"id\":\"${EMAIL_ID}\",\"name\":\"email\",\"providerId\":\"user-attribute-ldap-mapper\",\"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",\"parentId\":\"${LDAP_ID}\",\"config\":${EMAIL_CONFIG}}"
    echo "email ldap mapper updated to mailPrimaryAddress in realm ${REALM}"
  else
    curl -sf --max-time 30 -X POST -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
      "${KEYCLOAK_URL}/admin/realms/${REALM}/components" \
      -d "{\"name\":\"email\",\"providerId\":\"user-attribute-ldap-mapper\",\"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",\"parentId\":\"${LDAP_ID}\",\"config\":${EMAIL_CONFIG}}"
    echo "email ldap mapper created in realm ${REALM}"
  fi

  curl -sf --max-time 120 -X POST -H "${AUTH_HEADER}" \
    "${KEYCLOAK_URL}/admin/realms/${REALM}/user-storage/${LDAP_ID}/sync?action=triggerFullSync" >/dev/null 2>&1 || true
}
`
