#!/usr/bin/env bash

WORLD_BUNDLE_VERSION_RE='^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{5}$'
world_bundle_version_matches() { [[ "$1" =~ $WORLD_BUNDLE_VERSION_RE ]]; }
