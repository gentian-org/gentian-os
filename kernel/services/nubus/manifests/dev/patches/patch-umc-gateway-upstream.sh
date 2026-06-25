#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Patch nubus UMC gateway so Apache proxies /univention/oidc to umc-server
# (not 0.0.0.0:8090) and sets X-UMC-HTTPS=on behind TLS-terminating ingress.
#
# The upstream chart builds Apache config in the prepare-config init container;
# extraVolumeMounts on the main container are not enough. Helm upgrades reset
# the init command, so this script is re-run from the ArgoCD PostSync job
# (after Crossplane finishes) and manually via: ./update.sh --umc-gateway
set -euo pipefail

DEPLOY="${GENTIAN_UMC_GATEWAY_DEPLOY:-nubus-dev-umc-gateway}"
NS="${GENTIAN_NAMESPACE:-gentian-dev}"
WAIT_HELM_SEC="${GENTIAN_UMC_GATEWAY_WAIT_HELM_SEC:-120}"
MAX_ATTEMPTS="${GENTIAN_UMC_GATEWAY_PATCH_ATTEMPTS:-12}"

if ! kubectl get deployment "$DEPLOY" -n "$NS" >/dev/null 2>&1; then
  echo "deployment ${DEPLOY} not found in ${NS}; skipping"
  exit 0
fi

patch_deployment() {
  kubectl patch deployment "$DEPLOY" -n "$NS" --type=json -p '[
    {
      "op": "add",
      "path": "/spec/template/spec/initContainers/1/volumeMounts/-",
      "value": {
        "name": "gentian-umc-gateway-upstream",
        "mountPath": "/entrypoint.d/95-gentian-umc-gateway-upstream.sh",
        "subPath": "95-gentian-umc-gateway-upstream.sh"
      }
    }
  ]' 2>/dev/null || true

  kubectl patch deployment "$DEPLOY" -n "$NS" --type=json -p '[
    {
      "op": "add",
      "path": "/spec/template/spec/volumes/-",
      "value": {
        "name": "gentian-umc-gateway-upstream",
        "configMap": {
          "name": "nubus-dev-umc-gateway-upstream",
          "defaultMode": 493
        }
      }
    }
  ]' 2>/dev/null || true

  local init_cmd
  init_cmd=$(cat <<'CMD'
/entrypoint.d/50-entrypoint.sh
bash /entrypoint.d/95-gentian-umc-gateway-upstream.sh
sed -i 's|RequestHeader set X-UMC-HTTPS %{HTTPS}s|RequestHeader set X-UMC-HTTPS on|g' /etc/apache2/sites-available/univention.conf
echo "Listen 8080" > /etc/apache2/ports.conf
sed -e "s,<VirtualHost \*:80>,<VirtualHost *:8080>,g" -i /etc/apache2/sites-available/000-default.conf
cat /etc/apache2/sites-available/000-default.conf
CMD
)
  local cmd_patch
  cmd_patch=$(jq -n --arg cmd "$init_cmd" \
    '[{op: "replace", path: "/spec/template/spec/initContainers/1/command/2", value: $cmd}]')
  kubectl patch deployment "$DEPLOY" -n "$NS" --type=json -p "$cmd_patch"
  kubectl rollout status deployment/"$DEPLOY" -n "$NS" --timeout=180s
}

gateway_is_patched() {
  local cmd pod
  cmd=$(kubectl get deployment "$DEPLOY" -n "$NS" \
    -o jsonpath='{.spec.template.spec.initContainers[1].command[2]}' 2>/dev/null || true)
  [[ "$cmd" == *95-gentian-umc-gateway-upstream* ]] || return 1

  pod=$(kubectl get pods -n "$NS" \
    -l "app.kubernetes.io/name=umc-gateway,app.kubernetes.io/instance=nubus-dev" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null \
    | awk '{print $1}')
  [[ -n "$pod" ]] || return 1
  kubectl exec -n "$NS" "$pod" -- grep -q "umc-server-0" \
    /etc/apache2/sites-available/univention.conf 2>/dev/null
}

echo "Waiting ${WAIT_HELM_SEC}s for Crossplane/Helm to settle before UMC gateway patch..."
sleep "$WAIT_HELM_SEC"

attempt=1
while (( attempt <= MAX_ATTEMPTS )); do
  echo "UMC gateway patch attempt ${attempt}/${MAX_ATTEMPTS}..."
  patch_deployment
  if gateway_is_patched; then
    echo "UMC gateway upstream patch verified."
    exit 0
  fi
  echo "Patch not yet effective (Helm may have overwritten); retrying in 15s..."
  sleep 15
  attempt=$((attempt + 1))
done

echo "ERROR: UMC gateway upstream patch failed after ${MAX_ATTEMPTS} attempts" >&2
exit 1
