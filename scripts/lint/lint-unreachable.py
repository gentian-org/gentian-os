#!/usr/bin/env python3
"""Every shell function must be reachable from something that runs.

The companion to lint-resolvable.sh, which catches the opposite direction: a
call with no definition. This catches a definition with no call.

Both matter, and this one is the quieter failure. A function nothing reaches
costs nothing at runtime, so it survives every test, every install and every
review -- and then somebody reads it, believes it is how the thing works, and
changes it. Removing the v4 step set left fifty of these behind in one
afternoon: install_cert_manager, install_envoy_gateway, install_eso and their
private helpers, all describing an install that no longer happens.

## How reachability is decided

An ENTRY FILE is anything executed directly: install.sh, a step, a tool, a
test, a lint, kubectl-gentian. Every function name mentioned anywhere in one
is reachable. That over-approximates -- a name in a string counts -- and it is
deliberate: over-approximating keeps a function that might be live, and the
cost of being wrong in the other direction is an install that dies with
"command not found" at three in the morning.

A LIBRARY's top level (the code above its first definition) is reachable, and
so is every function reachable from a reachable function's body.

A BODY runs from its definition line to the next definition, not to a matching
brace. Counting braces in shell is a losing game: a jsonpath like
'{.status.conditions[?(@.type=="Ready")].status}' is not a block, and one
miscount swallows the rest of the file. Every definition in this repository
sits at column 0, so the next one is a boundary the text itself gives us.

## What it cannot see

A name built at runtime -- `"${verb}_thing"`, or a function extracted with sed
and eval'd. Two of those exist here and both name their functions literally,
so the scan finds them. `trap` handlers and `export -f` are seeded explicitly,
because the shell calls those and no caller appears anywhere: the EXIT trap in
portforward.sh read as dead until that was added.

If you add dispatch this cannot follow, seed it in ALWAYS_REACHABLE with a
comment saying why, rather than deleting the check.
"""

from __future__ import annotations

import collections
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
LIB = "scripts/lib/"

# Called by the driver on whichever step file it has sourced, so no caller for
# them appears anywhere in the corpus.
ALWAYS_REACHABLE = {"apply", "check", "destroy"}

DEF = re.compile(r"^([a-zA-Z_][a-zA-Z0-9_]*)\(\)\s*\{", re.M)
HEREDOC = re.compile(r"""<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\1""")
SUBST = re.compile(r"\$\(([^()]*(?:\([^()]*\)[^()]*)*)\)")
TRAP = re.compile(r"""^\s*trap\s+['"]?([a-zA-Z_][a-zA-Z0-9_]*)""", re.M)
EXPORTF = re.compile(r"^\s*export\s+-f\s+([a-zA-Z_][a-zA-Z0-9_]*)", re.M)

RED, GREEN, YELLOW, DIM, NC = "\033[0;31m", "\033[0;32m", "\033[1;33m", "\033[2m", "\033[0m"


def shell_only(path: pathlib.Path) -> str:
    """Comments are prose. A heredoc body is data -- unless its delimiter is
    unquoted, in which case $(fn) inside it is a live call, which is how the
    cluster claim is written."""
    out, inhd, delim, quoted = [], False, None, False
    for line in path.read_text(errors="replace").split("\n"):
        if inhd:
            if line.strip() == delim:
                inhd = False
            out.append("" if quoted else " ".join(SUBST.findall(line)))
            continue
        m = HEREDOC.search(line)
        if m and not line.lstrip().startswith("#"):
            quoted, delim, inhd = bool(m.group(1)), m.group(2), True
        out.append(re.sub(r"(^|\s)#.*$", "", line))
    return "\n".join(out)


def main() -> int:
    listed = subprocess.run(
        ["git", "ls-files", "*.sh", "scripts/kubectl-gentian", "install.sh"],
        capture_output=True, text=True, cwd=ROOT,
    ).stdout.split()
    text = {f: shell_only(ROOT / f) for f in listed}

    defs: dict[str, str] = {}
    for f, s in text.items():
        for m in DEF.finditer(s):
            defs.setdefault(m.group(1), f)

    bodies: dict[str, list[str]] = collections.defaultdict(list)
    toplevel: dict[str, str] = {}
    for f, s in text.items():
        lines = s.split("\n")
        starts = [i for i, line in enumerate(lines) if DEF.match(line)]
        toplevel[f] = "\n".join(lines[: starts[0]] if starts else lines)
        for k, i in enumerate(starts):
            j = starts[k + 1] if k + 1 < len(starts) else len(lines)
            bodies[DEF.match(lines[i]).group(1)].append("\n".join(lines[i:j]))

    names = set(defs)
    word = {n: re.compile(rf"(?<![A-Za-z0-9_]){re.escape(n)}(?![A-Za-z0-9_])") for n in names}

    def refs(blob: str, skip: str | None = None) -> set[str]:
        return {n for n in names if n != skip and word[n].search(blob)}

    stack = list(ALWAYS_REACHABLE & names)
    for f, s in text.items():
        stack.extend(refs(toplevel[f] if f.startswith(LIB) else s))
        for pattern in (TRAP, EXPORTF):
            stack.extend(m.group(1) for m in pattern.finditer(s) if m.group(1) in names)

    reach: set[str] = set()
    while stack:
        n = stack.pop()
        if n in reach:
            continue
        reach.add(n)
        for b in bodies.get(n, []):
            stack.extend(refs(b, skip=n))

    # A check that finds nothing to check passes forever. lint-bootstrap-apps
    # learned this the same way: the first version of this file compiled its
    # definition pattern without re.M, matched at the start of each file only,
    # found zero functions and reported every one of them reachable.
    if len(names) < 100:
        print(f"{RED}FAIL{NC} — found only {len(names)} function definitions in "
              f"{len(text)} files. This lint is reading the wrong thing and would "
              f"pass forever.")
        return 1

    dead = sorted(names - reach)
    print()
    print("Unreachable shell functions — is every definition called by something?")
    print(f"{DIM}{'─' * 60}{NC}")
    if not dead:
        print(f"{GREEN}Every function is reachable{NC} "
              f"({len(names)} definitions across {len(text)} files).")
        return 0

    by = collections.defaultdict(list)
    for n in dead:
        by[defs[n]].append(n)
    for f in sorted(by):
        for n in sorted(by[f]):
            print(f"  {RED}✗{NC} {f}: {n} is defined and never reached")
    print()
    print(f"{RED}{len(dead)} unreachable function(s).{NC} Delete them, or — if one is")
    print("called by something this cannot see — seed it in ALWAYS_REACHABLE with")
    print("a comment saying how it is reached.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
