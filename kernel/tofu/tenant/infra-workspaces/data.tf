# =============================================================================
# OpenBao data sources — infra-workspaces workspace
# =============================================================================
# nubus: used by nextcloud.tf (ldapsearch_nextcloud LDAP bind password).
# postfix/dovecot/ox data sources removed — those apps are now managed by the
# per-tenant operator (postfix-<tenant>, dovecot-<tenant>, tf-<tenant>-ox-appsuite).
# =============================================================================

data "vault_kv_secret_v2" "nubus" {
  mount = "secret"
  name  = "gentian-os/kernel/identity/nubus"
}

data "vault_kv_secret_v2" "postfix" {
  mount = "secret"
  name  = "gentian-os/kernel/mail/postfix"
}

data "vault_kv_secret_v2" "mail_smtp" {
  mount = "secret"
  name  = "gentian-os/kernel/mail/smtp"
}
