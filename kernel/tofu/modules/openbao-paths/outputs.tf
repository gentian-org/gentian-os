output "postgresql_path" {
  description = "Full KV v2 path for PostgreSQL passwords."
  value       = vault_kv_secret_v2.postgresql.path
}

output "mariadb_path" {
  description = "Full KV v2 path for MariaDB passwords."
  value       = vault_kv_secret_v2.mariadb.path
}

output "redis_path" {
  description = "Full KV v2 path for Redis passwords."
  value       = vault_kv_secret_v2.redis.path
}

output "minio_path" {
  description = "Full KV v2 path for MinIO passwords."
  value       = vault_kv_secret_v2.minio.path
}

output "nubus_path" {
  description = "Full KV v2 path for Nubus credentials."
  value       = vault_kv_secret_v2.nubus.path
}

output "keycloak_bootstrap_path" {
  description = "Full KV v2 path for Keycloak bootstrap credentials."
  value       = vault_kv_secret_v2.keycloak_bootstrap.path
}

output "intercom_path" {
  description = "Full KV v2 path for Intercom secrets."
  value       = vault_kv_secret_v2.intercom.path
}

output "nextcloud_path" {
  description = "Full KV v2 path for Nextcloud passwords."
  value       = vault_kv_secret_v2.nextcloud.path
}
