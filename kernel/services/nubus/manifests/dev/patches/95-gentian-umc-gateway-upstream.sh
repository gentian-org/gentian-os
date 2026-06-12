#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Point the UMC gateway Apache reverse proxy at the umc-server StatefulSet.
# umc/http/interface is the bind address for umc-server (0.0.0.0) and must not
# be set to the server Service hostname — patch Apache only on the gateway pod.
set -euo pipefail

SERVER="${GENTIAN_UMC_SERVER_HOST:-nubus-dev-umc-server-0.gentian-dev.svc.cluster.local}"
PORT="${GENTIAN_UMC_SERVER_PORT:-8090}"
UPSTREAM="http://${SERVER}:${PORT}"

APACHE="/etc/apache2/sites-available/univention.conf"
if [[ -f "${APACHE}" ]]; then
  sed -i "s|http://0.0.0.0:8090|${UPSTREAM}|g" "${APACHE}"
  sed -i "s|http://127.0.0.1:8090|${UPSTREAM}|g" "${APACHE}"
  sed -i "s|balancer://umcwebcluster|${UPSTREAM}|g" "${APACHE}"
fi
