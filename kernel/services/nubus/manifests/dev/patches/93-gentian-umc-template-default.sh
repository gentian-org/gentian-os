#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Patch UMC NewObjectDialog.js tenant user-template default selection.
# UMC matches add/default by template label. Prefer tenant "App User" and ignore
# any legacy kernel App User template DN still present in LDAP.
set -euo pipefail

FILE="/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js"
SENTINEL_V5="GENTIAN_TEMPLATE_DEFAULT_V5"

if [ ! -f "${FILE}" ]; then
  echo "ERROR: ${FILE} not found" >&2
  exit 1
fi

if grep -qF "${SENTINEL_V5}" "${FILE}"; then
  echo "UMC template default patch already present."
  exit 0
fi

if grep -qF "GENTIAN_TEMPLATE_DEFAULT_V4" "${FILE}"; then
  python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js")
content = path.read_text()
content = content.replace("GENTIAN_TEMPLATE_DEFAULT_V4", "GENTIAN_TEMPLATE_DEFAULT_V5", 1)
content = content.replace(
    "return ielement.label === '2 Admin User' || ielement.label === 'Admin User';",
    "return ielement.label === 'Admin User' || ielement.label === '2 Admin User';",
    1,
)
path.write_text(content)
print("Upgraded UMC NewObjectDialog.js template default patch to V5.")
PY
  exit 0
fi

if grep -qF "GENTIAN_TEMPLATE_DEFAULT_V3" "${FILE}"; then
  python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js")
content = path.read_text()
content = content.replace("GENTIAN_TEMPLATE_DEFAULT_V3", "GENTIAN_TEMPLATE_DEFAULT_V5", 1)
content = content.replace(
    "return ielement.label === '1 App User' || ielement.label === 'App User';",
    "return ielement.label === 'App User' || ielement.label === '1 App User';",
    1,
)
content = content.replace(
    "return ielement.label === '2 Admin User' || ielement.label === 'Admin User';",
    "return ielement.label === 'Admin User' || ielement.label === '2 Admin User';",
    1,
)
path.write_text(content)
print("Upgraded UMC NewObjectDialog.js template default patch to V5.")
PY
  exit 0
fi

if grep -qF "GENTIAN_TEMPLATE_DEFAULT_V2" "${FILE}"; then
  python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js")
content = path.read_text()
old = """\t\t\t\t\tif (appUser.length) {
\t\t\t\t\t\tdefaultTemplate = appUser[0].id;
\t\t\t\t\t} else if (visibleTemplates.length === 1) {
\t\t\t\t\t\tdefaultTemplate = visibleTemplates[0].id;
\t\t\t\t\t}"""
new = """\t\t\t\t\tif (appUser.length) {
\t\t\t\t\t\tdefaultTemplate = appUser[0].id;
\t\t\t\t\t} else {
\t\t\t\t\t\t// GENTIAN_TEMPLATE_DEFAULT_V5
\t\t\t\t\t\tvar adminUser = array.filter(visibleTemplates, function(ielement) {
\t\t\t\t\t\t\treturn ielement.label === 'Admin User' || ielement.label === '2 Admin User';
\t\t\t\t\t\t});
\t\t\t\t\t\tif (adminUser.length) {
\t\t\t\t\t\t\tdefaultTemplate = adminUser[0].id;
\t\t\t\t\t\t} else if (visibleTemplates.length === 1) {
\t\t\t\t\t\t\tdefaultTemplate = visibleTemplates[0].id;
\t\t\t\t\t\t}
\t\t\t\t\t}"""
if old not in content:
    raise SystemExit("ERROR: could not upgrade V2 template default patch")
path.write_text(content.replace(old, new, 1))
print("Upgraded UMC NewObjectDialog.js template default patch to V5.")
PY
  exit 0
fi

python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js")
content = path.read_text()

# Upgrade from V1 (single-template fallback only).
v1_anchor = "\t\t\t\tif (!defaultTemplate && templates.length === 1) {\n\t\t\t\t\tdefaultTemplate = templates[0].id;\n\t\t\t\t}"
v1_block = """\t\t\t\tif (initialValue) {
\t\t\t\t\tvar match = array.filter(templates, function(ielement) {
\t\t\t\t\t\treturn ielement.id == initialValue ||
\t\t\t\t\t\t\tielement.label.toLowerCase() == initialValue.toLowerCase();
\t\t\t\t\t});
\t\t\t\t\tif (match.length) {
\t\t\t\t\t\tdefaultTemplate = match[0].id;
\t\t\t\t\t}
\t\t\t\t}
\t\t\t\tif (!defaultTemplate && templates.length === 1) {
\t\t\t\t\tdefaultTemplate = templates[0].id;
\t\t\t\t}"""

v2_block = """\t\t\t\t// GENTIAN_TEMPLATE_DEFAULT_V5
\t\t\t\tvar kernelPlatform = 'cn=App User,cn=templates,cn=univention,';
\t\t\t\tvar visibleTemplates = array.filter(templates, function(ielement) {
\t\t\t\t\treturn ielement.id.indexOf(kernelPlatform) !== 0;
\t\t\t\t});
\t\t\t\tif (initialValue) {
\t\t\t\t\tvar match = array.filter(visibleTemplates, function(ielement) {
\t\t\t\t\t\treturn ielement.id == initialValue ||
\t\t\t\t\t\t\tielement.label.toLowerCase() == initialValue.toLowerCase();
\t\t\t\t\t});
\t\t\t\t\tif (match.length) {
\t\t\t\t\t\tdefaultTemplate = match[0].id;
\t\t\t\t\t}
\t\t\t\t}
\t\t\t\tif (!defaultTemplate) {
\t\t\t\t\tvar appUser = array.filter(visibleTemplates, function(ielement) {
\t\t\t\t\t\treturn ielement.label === 'App User' || ielement.label === '1 App User';
\t\t\t\t\t});
\t\t\t\t\tif (appUser.length) {
\t\t\t\t\t\tdefaultTemplate = appUser[0].id;
\t\t\t\t\t} else {
\t\t\t\t\t\tvar adminUser = array.filter(visibleTemplates, function(ielement) {
\t\t\t\t\t\t\treturn ielement.label === 'Admin User' || ielement.label === '2 Admin User';
\t\t\t\t\t\t});
\t\t\t\t\t\tif (adminUser.length) {
\t\t\t\t\t\t\tdefaultTemplate = adminUser[0].id;
\t\t\t\t\t\t} else if (visibleTemplates.length === 1) {
\t\t\t\t\t\t\tdefaultTemplate = visibleTemplates[0].id;
\t\t\t\t\t\t}
\t\t\t\t\t}
\t\t\t\t}"""

old_pristine = """\t\t\t\tif (initialValue) {
\t\t\t\t\tvar match = array.filter(templates, function(ielement) {
\t\t\t\t\t\treturn ielement.id == initialValue ||
\t\t\t\t\t\t\tielement.label.toLowerCase() == initialValue.toLowerCase();
\t\t\t\t\t});
\t\t\t\t\tif (match.length) {
\t\t\t\t\t\tdefaultTemplate = match[0].id;
\t\t\t\t\t}
\t\t\t\t}
\t\t\t}
\t\t\tthis._renderForm(defaultTemplate);"""

new_pristine = v2_block + """
\t\t\t}
\t\t\tthis._renderForm(defaultTemplate);"""

if v1_anchor in content:
    content = content.replace(v1_block, v2_block, 1)
elif old_pristine in content:
    content = content.replace(old_pristine, new_pristine, 1)
else:
    raise SystemExit("ERROR: could not locate NewObjectDialog.js patch anchor")

path.write_text(content)
print("Patched UMC NewObjectDialog.js template default selection.")
PY
