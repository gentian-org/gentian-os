#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Gentian patches for the UMC gateway Apache config (prepare-config init):
# 1. Point reverse-proxy upstream at the umc-server StatefulSet (not 0.0.0.0:8090).
# 2. Force X-UMC-HTTPS on so OIDC redirect_uri uses https behind TLS-terminating ingress.
set -euo pipefail

SERVER="${GENTIAN_UMC_SERVER_HOST:-nubus-dev-umc-server-0.gentian-dev.svc.cluster.local}"
PORT="${GENTIAN_UMC_SERVER_PORT:-8090}"
UPSTREAM="http://${SERVER}:${PORT}"

APACHE="/etc/apache2/sites-available/univention.conf"
if [[ ! -f "${APACHE}" ]]; then
  echo "WARN: ${APACHE} not found; skipping UMC gateway patch" >&2
  exit 0
fi
sed -i "s|http://0.0.0.0:8090|${UPSTREAM}|g" "${APACHE}"
sed -i "s|http://127.0.0.1:8090|${UPSTREAM}|g" "${APACHE}"
sed -i "s|balancer://umcwebcluster|${UPSTREAM}|g" "${APACHE}"
# Envoy/nginx terminate TLS; Apache sees plain HTTP so %{HTTPS}s is off. UMC uses
# X-UMC-HTTPS (same as opendesk nginx proxy_set_header / umc-server traefik middleware).
sed -i 's|RequestHeader set X-UMC-HTTPS %{HTTPS}s|RequestHeader set X-UMC-HTTPS on|g' "${APACHE}"
echo "Patched UMC gateway Apache upstream to ${UPSTREAM} and X-UMC-HTTPS=on"
