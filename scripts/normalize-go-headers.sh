#!/usr/bin/env bash
# Normalize Go file headers to hack/boilerplate.header.txt (Apache-2.0 block).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOILERPLATE="${ROOT}/hack/boilerplate.header.txt"
python3 - "$ROOT" "$BOILERPLATE" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
boiler = pathlib.Path(sys.argv[2]).read_text().rstrip()
correct_tail = (
    "Unless required by applicable law or agreed to in writing, software\n"
    "distributed under the License is distributed on an \"AS IS\" BASIS,\n"
    "WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.\n"
    "See the License for the specific language governing permissions and\n"
    "limitations under the License."
)
header_block = "/*\n" + boiler + "\n*/\n\n"
updated = 0

for path in sorted(root.rglob("*.go")):
    if "vendor" in path.parts or path.name.startswith("zz_generated"):
        continue
    text = path.read_text()
    orig = text

    if text.startswith("package "):
        text = header_block + text
    elif text.startswith("/*"):
        # Truncated header ending right after the license URL.
        text = re.sub(
            r"(?ms)^/\*\n.*?\n    http://www\.apache\.org/licenses/LICENSE-2\.0\n\*/\n",
            header_block,
            text,
            count=1,
        )
        # Minimal one-line copyright blocks.
        text = re.sub(
            r"(?ms)^/\*\nCopyright 2026 Gentian Organization\.\n\*/\n\n",
            header_block,
            text,
            count=1,
        )
        # Wrong single-line license tail before closing block comment.
        text = re.sub(
            r"(?ms)^WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied\.\nSee the License for[^\n]+\n\*/",
            "WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.\n"
            "See the License for the specific language governing permissions and\n"
            "limitations under the License.\n*/",
            text,
            count=1,
        )

    if text != orig:
        path.write_text(text)
        updated += 1

print(f"normalized {updated} Go file header(s)")
PY
