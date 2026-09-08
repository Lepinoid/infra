#!/usr/bin/env bash
# LepinoidTools bundle version (YYYY.MM.DD-<mcver>-<sha5>) のマッチ判定を共有する。
# mcver は 1.21.1 などの数字3桁だが将来 1.22 等も来る。sha5 は 5 桁小文字 hex。
BUNDLE_VERSION_RE='^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-1\.[0-9]+(\.[0-9]+)?-[0-9a-f]{5}$'
bundle_version_matches() {
  [[ "$1" =~ $BUNDLE_VERSION_RE ]]
}
