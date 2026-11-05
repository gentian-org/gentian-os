#!/usr/bin/env bash
# =============================================================================
# scripts/lib/log.sh — the installer's log lines: info, success, warn, error
# =============================================================================
# Self-contained, so a standalone script (scripts/bootstrap/init-openbao-transit.sh)
# prints the same lines as the steps without loading the whole library.
# scripts/lib/common.sh sources it for everything loaded through load.sh.
# =============================================================================

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
