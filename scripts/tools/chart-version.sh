#!/usr/bin/env bash
# chart-version.sh -- the version the charts of this commit are published under.
#
#   a release tag vX.Y.Z    X.Y.Z
#   any other commit        X.Y.Z-dev.<n>.g<sha7>
#
# A commit that is no release is a prerelease of the next patch after the
# newest release tag of the repository, so it sorts below that release and
# above the last one. <n> is the number of commits behind it: it only grows
# on a branch, so the builds of develop sort in the order they were made, and
# whoever follows develop takes the highest. The "g" keeps the commit an
# alphanumeric identifier: semver refuses digits alone with a leading zero.
#
# Needs the full history and the tags (actions/checkout with fetch-depth: 0).
set -euo pipefail

if [[ "${GITHUB_REF:-}" == refs/tags/v* ]]; then
    tag="${GITHUB_REF#refs/tags/}"
    [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
        || { echo "tag ${tag} is no version" >&2; exit 1; }
    echo "${tag#v}"
    exit 0
fi

if [[ "$(git rev-parse --is-shallow-repository)" == "true" ]]; then
    echo "this checkout is shallow: the commits cannot be counted" >&2
    exit 1
fi
newest="$(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1 || true)"
IFS=. read -r major minor patch <<<"${newest:-v0.0.0}"
sha="$(git rev-parse HEAD)"
echo "${major#v}.${minor}.$((patch + 1))-dev.$(git rev-list --count HEAD).g${sha::7}"
