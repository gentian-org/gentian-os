#!/usr/bin/env bash
# =============================================================================
# scripts/tools/verify-image-updates.sh — is the operator image actually moving?
# =============================================================================
# CI publishes an immutable tag for every build. Whether the cluster ever runs
# one is a separate question, and the failure mode is silent on both ends.
#
# argocd-image-updater reports success when it does nothing. An image it cannot
# resolve is skipped, not failed:
#
#   Processing results: applications=2 images_considered=2 images_skipped=1 \
#     images_updated=0 errors=0
#
# errors=0, condition *No errors*, Application Healthy, pods Running. The only
# visible symptom is a pod whose age keeps growing, which looks like stability.
# On 2026-08-21 the operator had been running a 17-hour-old image for a full day
# of merged fixes, and the tell was a retired Job that kept being recreated.
#
# lint-template-placeholders.py already guards the source: it catches the
# `${VAR}` in a Helm template that produced the original bug. This checks the
# other end, because those two can disagree indefinitely. The Application that
# carries these annotations is applied by the installer and never re-applied —
# the same "once" problem verify-claim-applied.sh exists for — so fixing the
# template fixes new installs and nothing else.
#
# Reads only. It reports; it does not patch, because the fix is a decision:
# an annotation patch repairs this cluster, re-running the installer repairs it
# the way the template intends, and only one of those is right for a given
# cluster.
#
# Usage:
#   scripts/tools/verify-image-updates.sh
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; DIM=$'\033[2m'; NC=$'\033[0m'

APP_NS="${GENTIAN_ARGOCD_NAMESPACE:-argocd}"
APP="${GENTIAN_OS_APPLICATION:-gentian-os}"
ANN="argocd-image-updater.argoproj.io"

for cmd in kubectl python3; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        echo "${YELLOW}SKIP${NC} — ${cmd} not found."
        exit 0
    fi
done
if ! kubectl cluster-info >/dev/null 2>&1; then
    echo "${YELLOW}SKIP${NC} — no reachable cluster."
    exit 0
fi
if ! kubectl -n "${APP_NS}" get application "${APP}" >/dev/null 2>&1; then
    echo "${YELLOW}SKIP${NC} — no Application ${APP} in ${APP_NS}."
    exit 0
fi

echo ""
echo "Image updates, as the cluster actually carries them"
echo "${DIM}────────────────────────────────────────────────────────────${NC}"

fail=0
warn=0

ann() {
    kubectl -n "${APP_NS}" get application "${APP}" \
        -o "jsonpath={.metadata.annotations.${ANN//./\\.}/$1}" 2>/dev/null
}

# ── 1. The annotation the updater parses ────────────────────────────────────
# A `${VAR}` here is the original defect. Helm does not expand shell syntax and
# nothing runs envsubst over that chart, so the literal reaches the cluster and
# the updater looks for a repository by that name, finds none, and skips it.
image_list="$(ann image-list)"
if [ -z "${image_list}" ]; then
    echo "  ${RED}FAIL${NC}  image-list annotation is absent — nothing is tracked."
    fail=1
elif printf '%s' "${image_list}" | grep -q '\${'; then
    echo "  ${RED}FAIL${NC}  image-list carries an unexpanded placeholder:"
    echo "        ${image_list}"
    echo "        ${DIM}Nothing expands it. The updater skips this image every cycle${NC}"
    echo "        ${DIM}and reports errors=0 while doing so.${NC}"
    fail=1
else
    echo "  ${GREEN}ok${NC}    image-list  ${image_list}"
fi

# ── 2. Which tags are candidates ────────────────────────────────────────────
# A moving tag can never resolve to a new value, so tracking one means image.tag
# never changes however many builds are published.
allow="$(ann gentianos\\.allow-tags)"
if [ -z "${allow}" ]; then
    echo "  ${YELLOW}warn${NC}  no allow-tags — every tag is a candidate, including"
    echo "        ${DIM}bare short-sha tags published from other branches.${NC}"
    warn=1
else
    echo "  ${GREEN}ok${NC}    allow-tags  ${allow}"
fi

# ── 3. Has the updater ever written back? ───────────────────────────────────
# image.tag is written by the updater, not the installer. Its absence means no
# update has ever landed, whatever the annotations say.
tag="$(kubectl -n "${APP_NS}" get application "${APP}" -o json 2>/dev/null | python3 -c '
import sys, json
d = json.load(sys.stdin)
srcs = d["spec"].get("sources") or [d["spec"].get("source", {})]
for s in srcs:
    for p in (s.get("helm", {}) or {}).get("parameters", []) or []:
        if p.get("name") == "image.tag":
            print(p.get("value", ""))
' 2>/dev/null)"
if [ -z "${tag}" ]; then
    echo "  ${RED}FAIL${NC}  no image.tag parameter — the updater has never written back,"
    echo "        ${DIM}so the chart default (a moving tag) is what deploys.${NC}"
    fail=1
else
    echo "  ${GREEN}ok${NC}    image.tag   ${tag}"
fi

# ── 4. What is actually running ─────────────────────────────────────────────
# The question the others only approximate.
running="$(kubectl get pods -A -l app.kubernetes.io/name="${APP}" \
    -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null)"
started="$(kubectl get pods -A -l app.kubernetes.io/name="${APP}" \
    -o jsonpath='{.items[0].status.startTime}' 2>/dev/null)"
if [ -n "${running}" ]; then
    echo "  ${DIM}      running     ${running}${NC}"
    echo "  ${DIM}      since       ${started}${NC}"
    case "${running}" in
        *:*[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) : ;;
        *)
            echo "  ${YELLOW}warn${NC}  the running image is a moving tag. A node that has cached"
            echo "        ${DIM}it never re-pulls, so 'Running' says nothing about the code.${NC}"
            warn=1
            ;;
    esac
fi

# ── 5. What the updater says it did ─────────────────────────────────────────
# Reported last, because it is the symptom the others explain.
upd_ns="$(kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -i image-updater | head -1 | awk '{print $1}')"
upd_pod="$(kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -i image-updater | head -1 | awk '{print $2}')"
if [ -n "${upd_pod}" ]; then
    last="$(kubectl -n "${upd_ns}" logs "${upd_pod}" --tail=200 2>/dev/null \
        | grep -o 'images_considered=[0-9]* images_skipped=[0-9]* images_updated=[0-9]* errors=[0-9]*' | tail -1)"
    if [ -n "${last}" ]; then
        echo "  ${DIM}      last cycle  ${last}${NC}"
        skipped="$(printf '%s' "${last}" | sed -n 's/.*images_skipped=\([0-9]*\).*/\1/p')"
        if [ "${skipped:-0}" -gt 0 ] 2>/dev/null; then
            echo "  ${YELLOW}warn${NC}  the updater is skipping ${skipped} image(s), and reports no error"
            echo "        ${DIM}for doing so.${NC}"
            warn=1
        fi
    fi
fi

echo "${DIM}────────────────────────────────────────────────────────────${NC}"
if [ "${fail}" -ne 0 ]; then
    echo "${RED}Image updates are not reaching this cluster.${NC}"
    echo "${DIM}CI can publish indefinitely without any of it being deployed.${NC}"
    exit 1
fi
if [ "${warn}" -ne 0 ]; then
    echo "${YELLOW}Image updates work, with caveats above.${NC}"
    exit 0
fi
echo "${GREEN}The cluster tracks the images CI publishes.${NC}"
