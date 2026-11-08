# crossplane-write: what provider-vault's token may do. __KV_MOUNT__ is the KV
# mount. The installer writes this on a new cluster (B-04), and vault-migrator
# writes it on one whose root token is gone (cmd/vault-migrator). This file is
# the one copy of the text.
# KV operations
path "__KV_MOUNT__/data/gentian-os/*"     { capabilities = ["create","read","update","delete"] }
path "__KV_MOUNT__/metadata/gentian-os/*" { capabilities = ["list","read","delete"] }
# Mount management (SecretMount MR)
path "sys/mounts/*"   { capabilities = ["create","read","update","delete","sudo"] }
path "sys/mounts"     { capabilities = ["read","list"] }
# Policy management (Policy MRs)
path "sys/policies/acl/*" { capabilities = ["create","read","update","delete","list"] }
path "sys/policies/acl"   { capabilities = ["read","list"] }
# Auth method management (Backend/BackendConfig/BackendRole MRs)
path "sys/auth/*"  { capabilities = ["create","read","update","delete","sudo"] }
# list, not just read — the same pair sys/mounts already gets above.
#
# OpenBao filters the sys/auth response by what the token may enumerate, so
# with read alone this token saw a table containing only token/ even though it
# could read sys/auth/oidc directly. provider-vault's AuthBackend finds its
# backend by enumerating that table, so the observe-only oidc AuthBackend in
# the Cluster composition reported "external resource does not exist" against
# a mount that demonstrably existed (accessor and all), stayed unSynced
# forever, and held the whole XCluster at Ready=False until B-08 timed out.
path "sys/auth"    { capabilities = ["read","list"] }
path "auth/+/config"  { capabilities = ["create","read","update"] }
path "auth/+/role/*"  { capabilities = ["create","read","update","delete","list"] }
# Token operations
path "auth/token/create"      { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
# The PKI roles the Cluster composition declares (SecretBackendRole), by prefix,
# so a new one needs no change here.
path "pki-internal/roles/platform-*" { capabilities = ["create","read","update","delete"] }
# Not gentian-ca, and not this policy. Both belong to vault-migrator, and this
# token writes every other policy, so each has to be said. These stop a mistake,
# not an attacker: this token can still write a new policy and bind it to a role.
path "pki-internal/roles/gentian-ca"      { capabilities = ["deny"] }
path "sys/policies/acl/crossplane-write"  { capabilities = ["read"] }
