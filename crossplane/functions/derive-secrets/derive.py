"""
Gentian OS — deterministic credential derivation.

Implements the same HMAC-SHA256 → SHA1 password derivation used by
scripts/seed-openbao.sh, expressed as a pure Python function that
can be unit-tested without external dependencies and later wrapped
in a Crossplane Composition Function image.

Algorithm (mirrors the bash reference):

    derive_password(master, context, purpose):
        message = f"{context}:{purpose}"   (UTF-8 bytes)
        hmac_bytes = HMAC-SHA256(key=master, msg=message)
        return SHA1(hmac_bytes).hexdigest()

    derive_nats_password(master, context, purpose):
        return "n" + derive_password(master, context, purpose)

Cross-checked against:
    echo -n "${context}:${purpose}" \\
      | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary \\
      | sha1sum | awk '{print $1}'
"""
from __future__ import annotations

import hashlib
import hmac


def derive_password(master_password: str, context: str, purpose: str) -> str:
    """Derive a deterministic credential string.

    Parameters
    ----------
    master_password:
        The cluster-wide master secret (kept in OpenBao, never in Git).
    context:
        The subsystem or component name, e.g. "postgres", "mariadb".
    purpose:
        The specific credential purpose, e.g. "postgres_user", "root_password".

    Returns
    -------
    str
        40-character hex string (SHA1 of the HMAC-SHA256 digest).
    """
    message = f"{context}:{purpose}".encode()
    key = master_password.encode()
    hmac_digest = hmac.new(key, message, hashlib.sha256).digest()
    return hashlib.sha1(hmac_digest).hexdigest()


def derive_nats_password(master_password: str, context: str, purpose: str) -> str:
    """Derive a NATS-compatible password (prefixed with 'n').

    NATS passwords must start with a letter; prefixing with 'n' ensures this
    without introducing bias, because SHA1 hex strings may start with a digit.
    """
    return "n" + derive_password(master_password, context, purpose)


# ---------------------------------------------------------------------------
# Canonical kernel credential map
#
# Maps (context, purpose) tuples — mirroring scripts/seed-openbao.sh — to the
# OpenBao path where the derived value should be stored.
# Used by the Crossplane Composition Function and the derive-secrets CLI.
# ---------------------------------------------------------------------------
KERNEL_CREDENTIALS: list[dict] = [
    # PostgreSQL (Bitnami / Nubus components)
    {"context": "postgres",  "purpose": "postgres_user",                  "path": "database/postgresql", "key": "postgres_password"},
    {"context": "postgres",  "purpose": "keycloak_user",                  "path": "database/postgresql", "key": "keycloak_user_password"},
    {"context": "postgres",  "purpose": "keycloak_extensions_user",       "path": "database/postgresql", "key": "keycloak_extensions_user_password"},
    {"context": "postgres",  "purpose": "selfservice_user",               "path": "database/postgresql", "key": "selfservice_user_password"},
    {"context": "postgres",  "purpose": "authsession_user",               "path": "database/postgresql", "key": "authsession_user_password"},
    {"context": "postgres",  "purpose": "guardianmanagementapi_user",     "path": "database/postgresql", "key": "guardianmanagementapi_user_password"},
    {"context": "postgres",  "purpose": "notificationsapi_user",          "path": "database/postgresql", "key": "notificationsapi_user_password"},
    {"context": "postgres",  "purpose": "nextcloud_user",                 "path": "database/postgresql", "key": "nextcloud_user_password"},
    # CloudNativePG superuser
    {"context": "cnpg",      "purpose": "superuser",                      "path": "database/cnpg",       "key": "superuser_password"},
    # MariaDB
    {"context": "mariadb",   "purpose": "root_password",                  "path": "database/mariadb",    "key": "root_password"},
    {"context": "mariadb",   "purpose": "openxchange_user",               "path": "database/mariadb",    "key": "openxchange_password"},
    # Redis
    {"context": "redis",     "purpose": "password",                       "path": "cache/redis",         "key": "auth_password"},
    # MinIO
    {"context": "minio",     "purpose": "root_password",                  "path": "storage/minio",       "key": "root_password"},
    {"context": "minio",     "purpose": "ums_user",                       "path": "storage/minio",       "key": "ums_password"},
    {"context": "minio",     "purpose": "nextcloud_user",                 "path": "storage/minio",       "key": "nextcloud_password"},
    {"context": "minio",     "purpose": "openxchange_user",               "path": "storage/minio",       "key": "openxchange_password"},
    {"context": "minio",     "purpose": "openproject_user",               "path": "storage/minio",       "key": "openproject_password"},
    # Keycloak
    {"context": "keycloak",  "purpose": "adminPassword",                  "path": "identity/keycloak-bootstrap", "key": "admin_password"},
    {"context": "keycloak",  "purpose": "intercom_client_secret",         "path": "identity/keycloak-bootstrap", "key": "intercom_client_secret"},
    # LDAP / Nubus
    {"context": "cn=admin",  "purpose": "ldap",                           "path": "identity/nubus",      "key": "ldap_admin_password"},
    {"context": "nubus",     "purpose": "Administrator",                  "path": "identity/nubus",      "key": "admin_password"},
    # Dovecot
    {"context": "dovecot",   "purpose": "doveadm_password",               "path": "mail/postfix",        "key": "doveadm_password"},
]
