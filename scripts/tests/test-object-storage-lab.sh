#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-object-storage-lab.sh
# =============================================================================
# The Jobs that move a tenant's objects, against the object store the
# platform deploys, in local containers.
#
# Builds the storage tools image (kernel/images/storage-tools) as CI does,
# starts the S3 server at the image the platform pins, and runs the Jobs the
# operator builds -- the bucket Job, a capture of a bucket and of a volume,
# the upload, the manifest, a restore of each, the deletion of a bundle and
# the purge of an app's bucket -- each container with the image, script,
# environment and volumes its Job gives it
# (internal/backup/objectstore_live_test.go). What counts is what the store
# holds afterwards, read back with rclone.
#
# Needs docker and go. Not part of CI: it builds an image and starts a server.
# =============================================================================
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

for tool in docker go; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "SKIP: ${tool} is not installed"; exit 0; }
done

image="gentian-storage-tools:lab-$$"
cleanup() { docker image rm -f "${image}" >/dev/null 2>&1 || :; }
trap cleanup EXIT

echo ""
echo "object storage: the Jobs against the server the platform deploys"
echo ""
docker build -q -t "${image}" kernel/images/storage-tools >/dev/null
GENTIAN_LIVE_S3=1 STORAGE_TOOLS_IMAGE="${image}" \
    go test ./internal/backup -run 'TestObjectStorageJobsAgainstAServer$' -count=1 -v
