# The tools every Job that moves a tenant's objects runs with: rclone, for the
# object store (plain S3, whichever server the cluster runs), and age, tar and
# coreutils, for the bundle each artefact becomes. Built in CI beside the
# operator and tagged as it is, so a Job runs the tools of the operator that
# made it. Nothing is fetched when a Job runs.
#
# util-linux-misc for script(1): age reads a passphrase only from a terminal,
# and script gives it one (internal/backup/encrypt.go).
FROM ghcr.io/rclone/rclone:1.75.2@sha256:2687085f718d3c628f7fdfb77c52a1d344332aed543110bb88088f2c70d43eb5 AS rclone

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache age coreutils tar gzip ca-certificates util-linux-misc
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
# No home directory to write a config to: every Job configures rclone
# through its environment (RCLONE_CONFIG_<REMOTE>_*).
ENV RCLONE_CONFIG=/dev/null
USER 65532
