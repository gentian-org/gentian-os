#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Point the UMC gateway Apache reverse proxy at the UMC server StatefulSet
# instead of localhost:8090 (nothing listens there in the gateway pod).
set -euo pipefail

SERVER="${GENTIAN_UMC_SERVER_HOST:-nubus-dev-umc-server-0.gentian-dev.svc.cluster.local}"
PORT="${GENTIAN_UMC_SERVER_PORT:-8090}"
UPSTREAM="http://${SERVER}:${PORT}"

if command -v ucr >/dev/null 2>&1; then
  ucr set "umc/http/interface=${SERVER}"
  ucr set "umc/http/port=${PORT}"
  ucr commit || true
fi

APACHE="/etc/apache2/sites-available/univention.conf"
if [[ -f "${APACHE}" ]]; then
  sed -i "s|http://0.0.0.0:8090|${UPSTREAM}|g" "${APACHE}"
  sed -i "s|http://127.0.0.1:8090|${UPSTREAM}|g" "${APACHE}"
  sed -i "s|balancer://umcwebcluster|${UPSTREAM}|g" "${APACHE}"
fi
