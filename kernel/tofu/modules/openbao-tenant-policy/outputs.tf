output "policy_name" {
  description = "Name of the OpenBao policy created for this tenant."
  value       = vault_policy.tenant_read.name
}

output "role_name" {
  description = "Name of the Kubernetes auth role created for this tenant."
  value       = vault_kubernetes_auth_backend_role.tenant.role_name
}
