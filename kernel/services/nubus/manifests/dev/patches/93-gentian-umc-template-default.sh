#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Patch UMC NewObjectDialog.js so tenant admins get a default user template when
# the global UCR default (kernel template DN) is not visible in LDAP (patch 11).
# UMC only matches add/default against template id/label (NewObjectDialog.js);
# when no match exists but exactly one template is available, select it.
#
# Idempotent: exits 0 without changes if the sentinel is already present.
set -euo pipefail

FILE="/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js"
SENTINEL="if (!defaultTemplate && templates.length === 1)"

if [ ! -f "${FILE}" ]; then
  echo "ERROR: ${FILE} not found" >&2
  exit 1
fi

if grep -qF "${SENTINEL}" "${FILE}"; then
  echo "UMC template default fallback already present."
  exit 0
fi

python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/NewObjectDialog.js")
content = path.read_text()
old = """\t\t\t\tif (initialValue) {
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
new = """\t\t\t\tif (initialValue) {
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
\t\t\t\t}
\t\t\t}
\t\t\tthis._renderForm(defaultTemplate);"""
if old not in content:
    raise SystemExit("ERROR: could not locate NewObjectDialog.js patch anchor")
path.write_text(content.replace(old, new, 1))
print("Patched UMC NewObjectDialog.js template default fallback.")
PY
