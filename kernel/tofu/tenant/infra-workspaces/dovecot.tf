# =============================================================================
# Dovecot — IMAP server for OX App Suite mail storage
# =============================================================================
# Deployed in the same phase as OX App Suite because ox-connector requires
# OX_IMAP_SERVER to be set for provisioning new user accounts.
#
# Chart: oci://registry.opencode.de/.../platform-development/charts/opendesk-dovecot
# Version: 3.4.1
#
# Required Vault secrets (provisioned by tofu/modules/openbao-paths):
#   secret/gentian-os/kernel/mail/dovecot:
#     doveadm_password          — doveadm admin password
#     oidc_client_secret        — Keycloak client secret for opendesk-dovecot
#   secret/gentian-os/kernel/identity/nubus:
#     ldapsearch_dovecot        — LDAP search bind password
# =============================================================================

data "vault_kv_secret_v2" "dovecot" {
  mount = "secret"
  name  = "gentian-os/kernel/mail/dovecot"
}

resource "helm_release" "dovecot" {
  name       = "opendesk-dovecot-${var.env}"
  repository = "oci://registry.opencode.de/bmi/opendesk/components/platform-development/charts/opendesk-dovecot"
  chart      = "dovecot"
  version    = "3.4.1"
  namespace  = "gentian-${var.env}"

  create_namespace = false
  wait             = true
  timeout          = 300

  values = [
    file("${path.module}/../../../apps/dovecot/values/_base.yaml"),
    file("${path.module}/../../../apps/dovecot/values/${var.env}/values-plain.yaml"),
    sensitive(templatefile(
      "${path.module}/../../../apps/dovecot/values/${var.env}/values-sensitive.yaml.tftpl",
      {
        doveadm_password        = data.vault_kv_secret_v2.dovecot.data["doveadm_password"]
        oidc_client_secret      = data.vault_kv_secret_v2.dovecot.data["oidc_client_secret"]
        ldap_dovecot_password   = data.vault_kv_secret_v2.nubus.data["ldapsearch_dovecot"]
      }
    )),
  ]
}
