# =============================================================================
# Nubus — Helm release with sensitive values from OpenBao
# =============================================================================
# Pattern B: masterPassword, SMTP passwords, keycloak.auth.password,
# NATS passwords (x5), templateContext passwords (x10), ldap admin password.
# PostgreSQL passwords are Pattern A (ESO ExternalSecret → nubus-credentials).
# =============================================================================

locals {
  nubus_root        = "${path.module}/../../../"
  nubus_env_values  = "${local.nubus_root}values/env/${var.env}.yaml"
  nubus_base_values = "${local.nubus_root}services/nubus/values/_base.yaml"
  nubus_plain       = "${local.nubus_root}services/nubus/values/${var.env}/values-plain.yaml"

  smtp_mode_raw = try(lower(data.vault_kv_secret_v2.mail_smtp.data["mode"]), "external")
  smtp_mode     = contains(["external", "kernel"], local.smtp_mode_raw) ? local.smtp_mode_raw : "external"

  smtp_host = local.smtp_mode == "kernel" ? "postfix-${var.env}.gentian-${var.env}.svc.cluster.local" : try(data.vault_kv_secret_v2.mail_smtp.data["host"], "")
  smtp_port = tonumber(try(data.vault_kv_secret_v2.mail_smtp.data["port"], "587"))

  smtp_ssl_raw      = try(lower(data.vault_kv_secret_v2.mail_smtp.data["ssl"]), "false")
  smtp_starttls_raw = try(lower(data.vault_kv_secret_v2.mail_smtp.data["starttls"]), "true")
  smtp_ssl          = local.smtp_ssl_raw == "true"
  smtp_starttls     = local.smtp_starttls_raw == "true"

  smtp_username = coalesce(
    try(data.vault_kv_secret_v2.mail_smtp.data["username"], null),
    try(data.vault_kv_secret_v2.postfix.data["relay_username"], null),
    "opendesk-system@${var.domain}"
  )

  nubus_smtp_password = local.smtp_mode == "external" ? try(data.vault_kv_secret_v2.postfix.data["relay_password"], data.vault_kv_secret_v2.nubus.data["smtp_password"]) : data.vault_kv_secret_v2.nubus.data["smtp_password"]
}

resource "helm_release" "nubus" {
  name       = "nubus-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/supplier/univention/charts-mirror"
  chart      = "nubus"
  version    = "1.16.0"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = false # nubus has long-running init jobs; don't wait for all pods
  timeout          = 900

  values = [
    file(local.nubus_env_values),
    templatefile(local.nubus_base_values, { domain = var.domain }),
    templatefile(local.nubus_plain, {
      smtp_host     = local.smtp_host,
      smtp_port     = local.smtp_port,
      smtp_ssl      = local.smtp_ssl,
      smtp_starttls = local.smtp_starttls,
      smtp_username = local.smtp_username,
    }),
  ]

  # Master password — used by nubus to derive other credentials internally
  set_sensitive {
    name  = "global.secrets.masterPassword"
    value = data.vault_kv_secret_v2.nubus.data["master_password"]
  }

  # SMTP auth passwords (UMC server and Keycloak extensions use the same password)
  set_sensitive {
    name  = "nubusUmcServer.smtp.auth.password"
    value = local.nubus_smtp_password
  }
  set_sensitive {
    name  = "nubusKeycloakExtensions.smtp.auth.password"
    value = local.nubus_smtp_password
  }

  # Keycloak admin password
  # NOTE: The keycloak subchart's secret-keycloak.yaml uses
  #   providedValues: ["keycloak.auth.password"] which resolves to
  #   .Values.keycloak.auth.password in the *subchart* context, i.e.
  #   keycloak.keycloak.auth.password at the *parent* (Nubus) chart level.
  # Setting keycloak.auth.password (subchart's .Values.auth.password) is a
  # no-op for the secret template and was previously ignored, causing the
  # nubus-dev-keycloak-credentials secret to use a Helm-derived value instead
  # of the canonical OpenBao password.
  set_sensitive {
    name  = "keycloak.keycloak.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["keycloak_admin_password"]
  }

  # Stack-data UMS template context passwords
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.initialPasswordAdministrator"
    value = data.vault_kv_secret_v2.nubus.data["admin_password"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.oxSystemUserPassword"
    value = data.vault_kv_secret_v2.nubus.data["ox_system_user_password"]
  }

  # LDAP search user passwords (indexed to match values-plain.yaml)
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[0].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_keycloak"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[1].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_nextcloud"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[2].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_dovecot"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[3].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_element"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[4].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_ox"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[5].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_postfix"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[6].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_openproject"]
  }
  set_sensitive {
    name  = "nubusStackDataUms.templateContext.ldapSearchUsers[7].password"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_xwiki"]
  }

  # NATS authentication passwords (must NOT start with a digit — prefixed 'n')
  set_sensitive {
    name  = "nubusUdmListener.nats.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["nats_udm_listener_password"]
  }
  set_sensitive {
    name  = "nubusProvisioning.api.nats.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["nats_api_password"]
  }
  set_sensitive {
    name  = "nubusProvisioning.dispatcher.nats.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["nats_dispatcher_password"]
  }
  set_sensitive {
    name  = "nubusProvisioning.udmTransformer.nats.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["nats_udm_transformer_password"]
  }
  set_sensitive {
    name  = "nubusProvisioning.prefill.nats.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["nats_prefill_password"]
  }

  # LDAP server admin password
  set_sensitive {
    name  = "nubusLdapServer.ldapServer.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["ldap_admin_password"]
  }

  # Provisioning consumer API passwords — stable Vault-derived values
  # Prevents Helm from regenerating these on upgrade (which would cause
  # NATS JetStream subscription drift and consumer CrashLoopBackOff).
  set_sensitive {
    name  = "nubusPortalConsumer.provisioningApi.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["portal_consumer_api_password"]
  }
  set_sensitive {
    name  = "nubusSelfServiceConsumer.provisioningApi.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["selfservice_consumer_api_password"]
  }
}

# =============================================================================
# UDM Listener NATS subject patch
# =============================================================================
# The provisioning-udm-listener image (0.66.x) ships a pydantic-v1 copy of
# backends/nats_mq.py with LDAP_SUBJECT = "ldap-producer-subject", but the
# backends module used by the udm-transformer defines LdapQueue.message_subject
# as "ldap-producer" (just the queue name, no "-subject" suffix).
#
# The transformer calls ensure_stream on startup with subjects=["ldap-producer"],
# overwriting the stream config. Subsequent listener publishes to the stale
# "ldap-producer-subject" are rejected with NoStreamResponseError.
#
# Fix: mount a patched mq_adapter_nats.py over the image copy via ConfigMap.
# The patch changes LDAP_SUBJECT to "ldap-producer" to match the transformer.
# =============================================================================
resource "kubernetes_config_map_v1" "nubus_udm_listener_nats_patch" {
  metadata {
    name      = "nubus-${var.env}-udm-listener-nats-patch"
    namespace = "gentian-${var.env}"
    labels = {
      "app.kubernetes.io/managed-by" = "Tofu"
      "app.kubernetes.io/part-of"    = "nubus"
    }
  }

  data = {
    "mq_adapter_nats.py" = file("${path.module}/../../../services/nubus/patches/mq_adapter_nats.py")
  }
}

