#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source ./world-bundle-version.sh

pass() { world_bundle_version_matches "$1" || { echo "FAIL: should match: $1"; exit 1; }; }
fail() { if world_bundle_version_matches "$1"; then echo "FAIL: should not match: $1"; exit 1; fi; }

pass '2026.09.10-e675f'
pass '2026.01.01-00000'
fail '2026.9.10-e675f'
fail '2026.09.10-1.21.8-abcde'
fail 'sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef'
fail ''
fail '2026.09.10-e675fg'
fail '2026-09-10-e675f'
echo "all world-bundle-version tests passed"
