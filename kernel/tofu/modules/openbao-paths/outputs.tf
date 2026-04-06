output "postgresql_path" {
  description = "Full KV v2 path for PostgreSQL passwords (gentian-os/kernel/database/postgresql)."
  value       = vault_kv_secret_v2.postgresql.path
}

output "mariadb_path" {
  description = "Full KV v2 path for MariaDB passwords (gentian-os/kernel/database/mariadb)."
  value       = vault_kv_secret_v2.mariadb.path
}

output "redis_path" {
  description = "Full KV v2 path for Redis passwords (gentian-os/kernel/cache/redis)."
  value       = vault_kv_secret_v2.redis.path
}

output "minio_path" {
  description = "Full KV v2 path for MinIO passwords (gentian-os/kernel/storage/minio)."
  value       = vault_kv_secret_v2.minio.path
}

output "nubus_path" {
  description = "Full KV v2 path for Nubus credentials (gentian-os/kernel/identity/nubus)."
  value       = vault_kv_secret_v2.nubus.path
}

output "keycloak_bootstrap_path" {
  description = "Full KV v2 path for Keycloak bootstrap credentials (gentian-os/kernel/identity/keycloak-bootstrap)."
  value       = vault_kv_secret_v2.keycloak_bootstrap.path
}

output "intercom_path" {
  description = "Full KV v2 path for Intercom secrets (gentian-os/kernel/identity/intercom)."
  value       = vault_kv_secret_v2.intercom.path
}

output "nextcloud_path" {
  description = "Full KV v2 path for Nextcloud passwords (gentian-os/kernel/apps/nextcloud)."
  value       = vault_kv_secret_v2.nextcloud.path
}

output "dovecot_path" {
  description = "Full KV v2 path for Dovecot credentials (gentian-os/kernel/mail/dovecot)."
  value       = vault_kv_secret_v2.dovecot.path
}

output "ox_path" {
  description = "Full KV v2 path for OX App Suite credentials (gentian-os/kernel/apps/ox)."
  value       = vault_kv_secret_v2.ox.path
}
