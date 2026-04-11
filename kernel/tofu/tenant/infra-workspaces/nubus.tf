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
    file(local.nubus_plain),
  ]

  # Master password — used by nubus to derive other credentials internally
  set_sensitive {
    name  = "global.secrets.masterPassword"
    value = data.vault_kv_secret_v2.nubus.data["master_password"]
  }

  # SMTP auth passwords (UMC server and Keycloak extensions use the same password)
  set_sensitive {
    name  = "nubusUmcServer.smtp.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["smtp_password"]
  }
  set_sensitive {
    name  = "nubusKeycloakExtensions.smtp.auth.password"
    value = data.vault_kv_secret_v2.nubus.data["smtp_password"]
  }

  # Keycloak admin password
  set_sensitive {
    name  = "keycloak.auth.password"
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

