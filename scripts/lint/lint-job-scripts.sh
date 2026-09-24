#!/usr/bin/env bash
# Syntax-check the shell scripts this repository embeds in Kubernetes Jobs.
#
# `bash -n` on the library proves the library parses. It says nothing about the
# script the library WRITES: that one lives as a YAML block scalar inside an
# unquoted heredoc, gets its escapes processed on the way out, and is then run
# by /bin/sh inside a container. Nothing checked it until it failed in a
# cluster.
#
# It failed for a small reason with a large cost. A comment inside a
# single-quoted jq program picked up an apostrophe:
#
#   -d "$(jq -n '{ # Keycloak's confirmation page ... }')"
#
# The shell does not know that `#` began a jq comment. It sees the quote close,
# the rest of the line become code, and the script dies at
# "syntax error: unexpected word (expecting \")\")" -- in the container, after
# the Job had already made half its changes, and only visible to somebody who
# went and read the pod logs.
#
# So: extract each embedded script exactly as the heredoc would emit it, and
# run `sh -n` over it. Cheap, and it catches the whole class rather than the
# one apostrophe.
set -euo pipefail

root="${SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
failed=0
checked=0

# Every library that embeds a Job script. Kept as a list rather than a glob so
# that adding one is a deliberate act with a name attached.
files=(
    "${root}/scripts/lib/portal-login-bootstrap.sh"
)

for file in "${files[@]}"; do
    [[ -f "${file}" ]] || continue
    # One temporary file per embedded block, numbered, so a failure names which.
    mapfile -t rendered < <(python3 - "${file}" <<'PY'
import pathlib, re, sys, tempfile

src = pathlib.Path(sys.argv[1]).read_text().splitlines()
# A block scalar introduced by a bare "- |" is a container's command argument.
for n, start in enumerate(i for i, l in enumerate(src) if l.strip() == "- |"):
    i = start + 1
    if i >= len(src):
        continue
    indent = len(src[i]) - len(src[i].lstrip())
    body = []
    while i < len(src):
        line = src[i]
        if line.strip() and (len(line) - len(line.lstrip())) < indent:
            break
        body.append(line[indent:] if len(line) >= indent else line)
        i += 1
    if not body:
        continue
    # What an UNQUOTED heredoc does to the text on its way out: these four
    # escapes are consumed, everything else is passed through untouched.
    script = re.sub(r'\\([$`\\\n])', r'\1', "\n".join(body))
    out = tempfile.NamedTemporaryFile(
        mode="w", suffix=f".job{n}.sh", delete=False, prefix="gentian-lint-")
    out.write(script)
    out.close()
    print(out.name)
PY
    )
    for script in "${rendered[@]}"; do
        checked=$((checked + 1))
        if ! sh -n "${script}" 2>/tmp/gentian-lint-job-err; then
            echo "FAIL ${file}: the script it writes into a Job does not parse." >&2
            sed 's/^/       /' /tmp/gentian-lint-job-err >&2
            echo "       Rendered script kept at ${script}" >&2
            echo "       A common cause is an apostrophe in a comment inside a" >&2
            echo "       single-quoted jq program, which closes the quote." >&2
            failed=1
        else
            rm -f "${script}"
        fi
    done
done

rm -f /tmp/gentian-lint-job-err
if [[ ${failed} -ne 0 ]]; then
    exit 1
fi
echo "Every embedded Job script parses (${checked} checked)."
