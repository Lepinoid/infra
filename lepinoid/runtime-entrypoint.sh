#!/bin/sh
set -eu
LEPINOID_PRESENCE_USER=$(cat /run/presence/username)
LEPINOID_PRESENCE_PASSWORD=$(cat /run/presence/password)
test -n "$LEPINOID_PRESENCE_USER"
test -n "$LEPINOID_PRESENCE_PASSWORD"
export LEPINOID_PRESENCE_USER LEPINOID_PRESENCE_PASSWORD
exec /bin/run-lepinoid-runtime "$@"
