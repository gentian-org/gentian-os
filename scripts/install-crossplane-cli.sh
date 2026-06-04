#!/usr/bin/env bash
# Install the Crossplane CLI binary into the current directory as ./crossplane.
#
# Crossplane core v2.2.x CLIs live on releases.crossplane.io (binary name: crank).
# v2.3+ CLIs are published to cli.crossplane.io (binary name: crossplane).
# Do not pipe crossplane/crossplane/main/install.sh here — its behavior tracks
# upstream main and has broken v2.2.1 installs when it pointed at cli.crossplane.io.
#
# Usage:
#   XP_VERSION=v2.2.1 ./scripts/install-crossplane-cli.sh
#   sudo mv crossplane /usr/local/bin/crossplane

set -euo pipefail

XP_VERSION="${XP_VERSION:-v2.2.1}"
_ver="${XP_VERSION#v}"
_major="${_ver%%.*}"
_minor_patch="${_ver#"${_major}".}"
_minor="${_minor_patch%%.*}"

os=$(uname -s)
arch=$(uname -m)
case "${os}" in
Linux)
	case "${arch}" in
	x86_64 | amd64) os_arch=linux_amd64 ;;
	arm64 | aarch64) os_arch=linux_arm64 ;;
	*) echo "Crossplane CLI: unsupported Linux arch ${arch}" >&2; exit 1 ;;
	esac
	;;
Darwin)
	case "${arch}" in
	x86_64 | amd64) os_arch=darwin_amd64 ;;
	arm64) os_arch=darwin_arm64 ;;
	*) echo "Crossplane CLI: unsupported Darwin arch ${arch}" >&2; exit 1 ;;
	esac
	;;
*)
	echo "Crossplane CLI: unsupported OS ${os}" >&2
	exit 1
	;;
esac

if [ "${_major}" -lt 2 ] || { [ "${_major}" -eq 2 ] && [ "${_minor}" -lt 3 ]; }; then
	url="https://releases.crossplane.io/stable/v${_ver}/bin/${os_arch}/crank"
else
	url="https://cli.crossplane.io/stable/v${_ver}/bin/${os_arch}/crossplane"
fi

if ! curl -sfL "${url}" -o crossplane; then
	echo "Failed to download Crossplane CLI v${_ver} from ${url}" >&2
	exit 1
fi

chmod +x crossplane
echo "crossplane CLI v${_ver} downloaded ($(pwd)/crossplane)"
