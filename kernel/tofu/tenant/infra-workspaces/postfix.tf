# =============================================================================
# Postfix — Helm release with sensitive values from OpenBao
# =============================================================================
# Pattern B: relay credentials (Brevo) and internal auth password injected
# via set_sensitive. LDAP search password reuses the nubus data source.
#
# Required Vault secrets (write before applying):
#   bao kv put secret/gentian/dev/postfix \
#     relay_username="<brevo-login-email>" \
#     relay_password="<brevo-smtp-key>"
#
# Internal SMTP auth password (opendesk-system) reuses nubus.smtp_password.
# LDAP search password reuses nubus.ldapsearch_postfix.
# =============================================================================

locals {
  postfix_root  = "${path.module}/../../../"
  postfix_base  = "${local.postfix_root}services/postfix/values/_base.yaml"
  postfix_plain = "${local.postfix_root}services/postfix/values/${var.env}/values-plain.yaml"
}

resource "helm_release" "postfix" {
  name       = "postfix-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-postfix"
  chart      = "postfix"
  version    = "5.2.0"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file(local.postfix_base),
    file(local.postfix_plain),
  ]

  # Gmail SMTP relay credentials (Vault: gentian/{env}/postfix)
  set_sensitive {
    name  = "postfix.relayHost.authentication.username.value"
    value = data.vault_kv_secret_v2.postfix.data["relay_username"]
  }
  set_sensitive {
    name  = "postfix.relayHost.authentication.password.value"
    value = data.vault_kv_secret_v2.postfix.data["relay_password"]
  }

  # Internal SMTP auth — same password nubus UMC uses to authenticate with Postfix
  set_sensitive {
    name  = "postfix.staticAuthDB.password.value"
    value = data.vault_kv_secret_v2.nubus.data["smtp_password"]
  }

  # LDAP bind password for virtual alias lookups
  set_sensitive {
    name  = "postfix.ldap.password.value"
    value = data.vault_kv_secret_v2.nubus.data["ldapsearch_postfix"]
  }
}
