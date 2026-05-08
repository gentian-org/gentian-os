variable "domain" {
  description = "Base domain for all service FQDNs (e.g. desk.gentian.org). Used to construct the UDM REST API URL."
  type        = string
  default     = "desk.gentian.org"
}

variable "env" {
  description = "Environment name (dev, staging, prod). Affects OpenBao secret paths."
  type        = string
  default     = "dev"
}

variable "ldap_base" {
  description = "LDAP base DN used by the Nubus deployment."
  type        = string
  default     = "dc=swp-ldap,dc=internal"
}
