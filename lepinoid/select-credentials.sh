#!/bin/sh
set -eu
case "$LEPINOID_INSTANCE_ID" in
  lepinoid-runtime-pool-0|lepinoid-runtime-pool-1|lepinoid-runtime-pool-2) ;;
  *) echo 'Unsupported pool ordinal' >&2; exit 1 ;;
esac
ordinal=${LEPINOID_INSTANCE_ID##*-}
umask 077
test -s "/run/all-presence/presence-$ordinal.username"
test -s "/run/all-presence/presence-$ordinal.password"
cp "/run/all-presence/presence-$ordinal.username" /run/presence/username
cp "/run/all-presence/presence-$ordinal.password" /run/presence/password
chmod 0600 /run/presence/username /run/presence/password
