output "client_uuid" {
  description = "Keycloak internal UUID of the created OIDC client. Use this for import blocks and audience mappers."
  value       = keycloak_openid_client.this.id
}

output "client_id" {
  description = "The client_id string as configured in Keycloak."
  value       = keycloak_openid_client.this.client_id
}

output "client_secret_path" {
  description = "Full OpenBao KV v2 path where the client_secret is stored."
  value       = vault_kv_secret_v2.client_secret.path
}
