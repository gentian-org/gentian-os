#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Gentian GmbH
#
# Patch FirstPageWizard.js for tenant user creation:
# - Hide legacy kernel App User template (cn=App User under cn=univention), if any
# - Sort templates by label (use "1 App User", "2 Admin User" prefixes)
# - Show "None" last (append to dynamic list, clear staticValues)
set -euo pipefail

FILE="/usr/share/univention-management-console-frontend/js/umc/modules/udm/wizards/FirstPageWizard.js"
SENTINEL="GENTIAN_TEMPLATE_PICKER_V1"

if [ ! -f "${FILE}" ]; then
  echo "ERROR: ${FILE} not found" >&2
  exit 1
fi

if grep -qF "${SENTINEL}" "${FILE}"; then
  echo "UMC template picker patch already present."
  exit 0
fi

python3 <<'PY'
from pathlib import Path

path = Path("/usr/share/univention-management-console-frontend/js/umc/modules/udm/wizards/FirstPageWizard.js")
content = path.read_text()
old = """\t\t\t\t\tdynamicValues: lang.hitch(this, function(options) {
\t\t\t\t\t\treturn this.moduleCache.getTemplates(options.objectType).then(function(result) {
\t\t\t\t\t\t\tresult.sort(tools.cmpObjects('label'));
\t\t\t\t\t\t\treturn result;
\t\t\t\t\t\t});
\t\t\t\t\t}),
\t\t\t\t\tstaticValues: [{id: 'None', label: _('None')}]"""
new = """\t\t\t\t\tdynamicValues: lang.hitch(this, function(options) {
\t\t\t\t\t\treturn this.moduleCache.getTemplates(options.objectType).then(lang.hitch(this, function(result) {
\t\t\t\t\t\t\t// GENTIAN_TEMPLATE_PICKER_V1: hide kernel Platform App User template
\t\t\t\t\t\t\tvar kernelPlatform = 'cn=App User,cn=templates,cn=univention,';
\t\t\t\t\t\t\tresult = array.filter(result, function(t) {
\t\t\t\t\t\t\t\treturn t.id.indexOf(kernelPlatform) !== 0;
\t\t\t\t\t\t\t});
\t\t\t\t\t\t\tresult.sort(tools.cmpObjects('label'));
\t\t\t\t\t\t\tresult.push({id: 'None', label: _('None')});
\t\t\t\t\t\t\treturn result;
\t\t\t\t\t\t}));
\t\t\t\t\t}),
\t\t\t\t\tstaticValues: []"""
if old not in content:
    raise SystemExit("ERROR: could not locate FirstPageWizard.js patch anchor")
path.write_text(content.replace(old, new, 1))
print("Patched UMC FirstPageWizard.js template picker.")
PY
