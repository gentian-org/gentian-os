# =============================================================================
# app-workspace — input variables
# =============================================================================
# Received from the gentian-os operator via the Terraform CR's spec.vars and
# spec.varsFrom fields. The operator sets vm_* variables from the AppProfile's
# valueMapping, which maps service credential keys to Helm value paths.
# =============================================================================

# ── Core identifiers ──────────────────────────────────────────────────────────

variable "tenant_name" {
  description = "Tenant name (Tenant CR .metadata.name). Used to derive OpenBao secret paths."
  type        = string
}

variable "app_name" {
  description = "App profile name (AppProfile CR .metadata.name). Used to derive OpenBao secret paths."
  type        = string
}

variable "namespace" {
  description = "Target Kubernetes namespace for the helm_release (e.g. tenant-gtn-demo)."
  type        = string
}

# ── Helm chart reference ───────────────────────────────────────────────────────

variable "chart_repository" {
  description = "Helm chart repository URL (OCI or HTTP). E.g. oci://registry.example.com/charts."
  type        = string
}

variable "chart_name" {
  description = "Helm chart name."
  type        = string
}

variable "chart_version" {
  description = "Helm chart version to deploy."
  type        = string
}

# ── Registry credentials (sensitive — injected via spec.varsFrom Secret) ──────

variable "registry_username" {
  description = "Username for the Helm chart OCI registry."
  type        = string
  sensitive   = true
  default     = ""
}

variable "registry_password" {
  description = "Password/token for the Helm chart OCI registry."
  type        = string
  sensitive   = true
  default     = ""
}

# ── Non-sensitive extra values ────────────────────────────────────────────────

variable "extra_values_json" {
  description = "JSON string of non-sensitive extra Helm values (from AppProfile.spec.extraValues and tenant replica overrides). Merged into the helm_release values list."
  type        = string
  default     = ""
}

# ── OIDC value-mapping keys ───────────────────────────────────────────────────
# These are the Helm value paths where the module injects credentials read from
# OpenBao (gentian-os/tenants/{tenant}/apps/{app}/oidc). Empty string = unused.

variable "vm_oidc_issuer_key" {
  description = "Helm value key for the OIDC issuer URL."
  type        = string
  default     = ""
}

variable "vm_oidc_client_id_key" {
  description = "Helm value key for the OIDC client ID."
  type        = string
  default     = ""
}

variable "vm_oidc_client_secret_key" {
  description = "Helm value key for the OIDC client secret."
  type        = string
  default     = ""
}

# ── Database value-mapping keys ───────────────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/database

variable "vm_db_host_key" {
  description = "Helm value key for the database host."
  type        = string
  default     = ""
}

variable "vm_db_port_key" {
  description = "Helm value key for the database port."
  type        = string
  default     = ""
}

variable "vm_db_name_key" {
  description = "Helm value key for the database name."
  type        = string
  default     = ""
}

variable "vm_db_user_key" {
  description = "Helm value key for the database user."
  type        = string
  default     = ""
}

variable "vm_db_password_key" {
  description = "Helm value key for the database password."
  type        = string
  default     = ""
}

# ── S3 / object storage value-mapping keys ────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/s3

variable "vm_s3_endpoint_key" {
  description = "Helm value key for the S3 endpoint URL."
  type        = string
  default     = ""
}

variable "vm_s3_bucket_key" {
  description = "Helm value key for the S3 bucket name."
  type        = string
  default     = ""
}

variable "vm_s3_access_key_key" {
  description = "Helm value key for the S3 access key ID."
  type        = string
  default     = ""
}

variable "vm_s3_secret_key_key" {
  description = "Helm value key for the S3 secret access key."
  type        = string
  default     = ""
}

variable "vm_s3_region_key" {
  description = "Helm value key for the S3 region."
  type        = string
  default     = ""
}

# ── Cache value-mapping keys ───────────────────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/cache

variable "vm_cache_host_key" {
  description = "Helm value key for the cache host."
  type        = string
  default     = ""
}

variable "vm_cache_port_key" {
  description = "Helm value key for the cache port."
  type        = string
  default     = ""
}

variable "vm_cache_password_key" {
  description = "Helm value key for the cache password."
  type        = string
  default     = ""
}

# ── SMTP value-mapping keys ────────────────────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/smtp

variable "vm_smtp_host_key" {
  description = "Helm value key for the SMTP host."
  type        = string
  default     = ""
}

variable "vm_smtp_port_key" {
  description = "Helm value key for the SMTP port."
  type        = string
  default     = ""
}

variable "vm_smtp_user_key" {
  description = "Helm value key for the SMTP user."
  type        = string
  default     = ""
}

variable "vm_smtp_password_key" {
  description = "Helm value key for the SMTP password."
  type        = string
  default     = ""
}

# ── IMAP value-mapping keys ────────────────────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/imap

variable "vm_imap_host_key" {
  description = "Helm value key for the IMAP host."
  type        = string
  default     = ""
}

variable "vm_imap_port_key" {
  description = "Helm value key for the IMAP port."
  type        = string
  default     = ""
}

# ── LDAP value-mapping keys ────────────────────────────────────────────────────
# OpenBao path: gentian-os/tenants/{tenant}/apps/{app}/ldap

variable "vm_ldap_host_key" {
  description = "Helm value key for the LDAP host."
  type        = string
  default     = ""
}

variable "vm_ldap_port_key" {
  description = "Helm value key for the LDAP port."
  type        = string
  default     = ""
}

variable "vm_ldap_base_dn_key" {
  description = "Helm value key for the LDAP base DN."
  type        = string
  default     = ""
}

variable "vm_ldap_bind_dn_key" {
  description = "Helm value key for the LDAP bind DN."
  type        = string
  default     = ""
}

variable "vm_ldap_bind_password_key" {
  description = "Helm value key for the LDAP bind password."
  type        = string
  default     = ""
}

# ── Per-app internal secrets (Inc 21a) ─────────────────────────────────────────
# Map from AppProfile.spec.appSecrets[].name → AppProfile.spec.appSecrets[].valuePath.
# The operator populates this from the AppProfile on every reconcile. Each key
# is read from secret/data/gentian-os/tenants/{tenant}/apps/{app}/internal/{name}
# (single-field KV record with key "value") and merged into the Helm release as
# a sensitive value at the given dot path. This is how app-specific secrets
# (e.g. Synapse registration_shared_secret, bridge tokens) reach the chart
# without any per-app Terraform module.

variable "app_secrets" {
  description = "Map of app-internal secret name → Helm value dot path. Secrets are read from {secret_base}/internal/{name} with key \"value\"."
  type        = map(string)
  default     = {}
}
