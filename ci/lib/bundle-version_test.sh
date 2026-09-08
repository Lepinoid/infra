#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source ./bundle-version.sh

pass() { bundle_version_matches "$1" || { echo "FAIL: should match: $1"; exit 1; }; }
fail() { if bundle_version_matches "$1"; then echo "FAIL: should not match: $1"; exit 1; fi; }

pass '2026.09.07-1.21.1-17a4c'
pass '2026.09.09-1.21.8-abcde'
pass '2027.01.01-1.22-00000'
fail '2026.09.09-1.21.8-abcd'
fail '2026.9.9-1.21.8-abcde'
fail 'sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef'
fail '2026.09.07-1.21'
fail ''
echo "all bundle-version tests passed"
