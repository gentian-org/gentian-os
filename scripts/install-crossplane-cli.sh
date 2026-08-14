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

# shellcheck source=scripts/lib/versions.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/lib" && pwd)/versions.sh"
XP_VERSION="${XP_VERSION:-$(gentian_pin crossplane cli)}"
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

use_legacy_host=false
if [ "${_major}" -lt 2 ] || { [ "${_major}" -eq 2 ] && [ "${_minor}" -lt 3 ]; }; then
	use_legacy_host=true
fi

curl_fetch() {
	local dest="$1"
	shift
	local url
	for url in "$@"; do
		if curl -sfL \
			--retry 5 --retry-delay 2 --retry-all-errors \
			--connect-timeout 30 --max-time 600 \
			"${url}" -o "${dest}"; then
			return 0
		fi
	done
	return 1
}

verify_cli_binary() {
	local path="$1"
	[ -f "${path}" ] && [ -s "${path}" ] && [ -x "${path}" ] || return 1
	# Reject HTML error pages masquerading as binaries.
	head -c 4 "${path}" | grep -q $'\x7fELF' && return 0
	head -c 4 "${path}" | grep -q $'\xcf\xfa\xed\xfe' && return 0 # Mach-O
	return 1
}

install_from_releases() {
	local host url bin_name
	if [ "${use_legacy_host}" = true ]; then
		host="releases.crossplane.io"
		bin_name="crank"
	else
		host="cli.crossplane.io"
		bin_name="crossplane"
	fi

	# Prefer uncompressed binary; fall back to upstream bundle tarball.
	local urls=(
		"https://${host}/stable/v${_ver}/bin/${os_arch}/${bin_name}"
		"https://${host}/stable/v${_ver}/bundle/${os_arch}/${bin_name}.tar.gz"
	)
	if [ "${bin_name}" = "crank" ]; then
		urls+=("https://${host}/stable/v${_ver}/bin/${os_arch}/crossplane")
	fi

	local tmp
	tmp=$(mktemp)
	if ! curl_fetch "${tmp}" "${urls[@]}"; then
		rm -f "${tmp}"
		return 1
	fi

	if tar tzf "${tmp}" >/dev/null 2>&1; then
		tar xzf "${tmp}" -C .
		rm -f "${tmp}"
		if [ -f "${bin_name}" ]; then
			mv -f "${bin_name}" crossplane
			chmod +x crossplane
			rm -f "${bin_name}.sha256" 2>/dev/null || true
			verify_cli_binary crossplane
			return $?
		fi
		rm -f "${bin_name}" "${bin_name}.sha256" 2>/dev/null || true
		return 1
	fi

	mv -f "${tmp}" crossplane
	chmod +x crossplane
	verify_cli_binary crossplane
}

install_via_go() {
	command -v go >/dev/null 2>&1 || return 1
	local mod="github.com/crossplane/crossplane/v2/cmd/crank"
	if [ "${use_legacy_host}" = false ]; then
		mod="github.com/crossplane/cli/v2/cmd/crossplane"
	fi
	local gopath gomodcache
	gopath=$(mktemp -d)
	gomodcache="${gopath}/modcache"
	mkdir -p "${gomodcache}"
	if ! GOPATH="${gopath}" GOMODCACHE="${gomodcache}" GOBIN="$(pwd)" GOTOOLCHAIN=auto \
		go install "${mod}@${XP_VERSION}"; then
		chmod -R u+w "${gopath}" 2>/dev/null || true
		rm -rf "${gopath}"
		return 1
	fi
	chmod -R u+w "${gopath}" 2>/dev/null || true
	rm -rf "${gopath}"
	if [ -f crank ] && [ ! -f crossplane ]; then
		mv -f crank crossplane
	fi
	chmod +x crossplane 2>/dev/null || true
	verify_cli_binary crossplane
}

rm -f crossplane

attempt=1
max_attempts=3
while [ "${attempt}" -le "${max_attempts}" ]; do
	if install_from_releases; then
		echo "crossplane CLI v${_ver} downloaded ($(pwd)/crossplane)"
		exit 0
	fi
	if [ "${attempt}" -lt "${max_attempts}" ]; then
		echo "Crossplane CLI: release download failed (attempt ${attempt}/${max_attempts}), retrying..." >&2
		sleep 2
	fi
	attempt=$((attempt + 1))
done

echo "Crossplane CLI: release download failed, trying go install..." >&2
if install_via_go; then
	echo "crossplane CLI v${_ver} built with go install ($(pwd)/crossplane)"
	exit 0
fi

echo "Failed to install Crossplane CLI v${_ver} (releases.crossplane.io and go install)" >&2
exit 1
