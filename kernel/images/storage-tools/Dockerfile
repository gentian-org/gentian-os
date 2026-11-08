# The tools every Job that moves a tenant's objects runs with: rclone, for the
# object store (plain S3, whichever server the cluster runs), and age, tar and
# coreutils, for the bundle each artefact becomes. curl and jq, for the Jobs
# that provision, export and import a tenant's identity realm through the
# identity provider's administration API. Built in CI beside the operator and
# tagged as it is, so a Job runs the tools of the operator that made it.
# Nothing is fetched or installed when a Job runs, and every Job runs it as
# the user below.
#
# util-linux-misc for script(1): age reads a passphrase only from a terminal,
# and script gives it one (internal/backup/encrypt.go).
FROM ghcr.io/rclone/rclone:1.76.0@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466 AS rclone

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache age coreutils tar gzip ca-certificates util-linux-misc curl jq
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
# No home directory to write a config to: every Job configures rclone
# through its environment (RCLONE_CONFIG_<REMOTE>_*).
ENV RCLONE_CONFIG=/dev/null
USER 65532
