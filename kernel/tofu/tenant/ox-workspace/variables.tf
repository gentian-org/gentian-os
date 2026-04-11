# =============================================================================
# ox-workspace — input variables
# =============================================================================
# Received from the gentian-os operator via the Terraform CR's spec.vars and
# spec.varsFrom fields. This module is dedicated to OX App Suite because the
# chart requires credentials in deeply nested propertiesFiles["/path"].key
# structures that cannot be expressed as individual Helm --set values.
# Instead, a templatefile() renders a sensitive YAML values file.
# =============================================================================

variable "domain" {
  description = "Base domain for all service FQDNs (e.g. desk.gentian.org). Used by templatefile() to render _base.yaml and in ox-connector set blocks."
  type        = string
  default     = "desk.gentian.org"
}

variable "tenant_name" {
  description = "Tenant name (Tenant CR .metadata.name). Used to derive OpenBao secret paths."
  type        = string
}

variable "app_name" {
  description = "App profile name (AppProfile CR .metadata.name). Used to derive OpenBao secret paths."
  type        = string
  default     = "ox-appsuite"
}

variable "namespace" {
  description = "Target Kubernetes namespace for the helm_release (e.g. tenant-gtn-demo)."
  type        = string
}

# ── Helm chart reference (passed from AppProfile.spec.chart) ─────────────────

variable "chart_repository" {
  description = "Helm chart repository URL. Should be oci://registry.opencode.de/bmi/opendesk/components/supplier/open-xchange/charts-mirror"
  type        = string
}

variable "chart_name" {
  description = "Helm chart name. Should be appsuite-public-sector."
  type        = string
}

variable "chart_version" {
  description = "Helm chart version to deploy. Aligned with opendesk charts.yaml.gotmpl."
  type        = string
}

# ── Registry credentials (sensitive — injected via spec.varsFrom Secret) ──────

variable "registry_username" {
  description = "Username for the OCI registry at registry.opencode.de."
  type        = string
  sensitive   = true
  default     = ""
}

variable "registry_password" {
  description = "Password/token for the OCI registry at registry.opencode.de."
  type        = string
  sensitive   = true
  default     = ""
}

# ── Non-sensitive extra values ────────────────────────────────────────────────

variable "extra_values_json" {
  description = "JSON string of non-sensitive extra Helm values (from AppProfile.spec.extraValues). Merged first so sensitive templatefile() values override them."
  type        = string
  default     = ""
}
