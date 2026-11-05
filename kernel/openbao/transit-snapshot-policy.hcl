# The seal's daily snapshot (docs/guides/recovery.md §4): the role
# openbao-transit-snapshot reads a Raft snapshot of the seal and nothing else.
# Written by scripts/bootstrap/init-openbao-transit.sh on a new seal, and by
# hand on a moved one (docs/guides/recovery.md §5).
path "sys/storage/raft/snapshot" {
  capabilities = ["read"]
}
