#!/usr/bin/env bash

world_config_extract_json() {
  awk '
    /^  config\.json: \|-?/ { found = 1; next }
    found && /^    / { sub(/^    /, ""); print; next }
    found { exit }
  ' "$1"
}

world_config_enabled_names() {
  local json
  json=$(world_config_extract_json "$1") || return 1

  jq -e '
    (keys == ["schemaVersion", "worlds"])
    and (.schemaVersion == 1)
    and (.worlds | type == "array" and length > 0)
    and (all(.worlds[]; (keys == ["enabled", "name"])
      and (.name | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"))
      and (.enabled | type == "boolean")))
    and (([.worlds[].name] | unique | length) == (.worlds | length))
  ' <<<"$json" >/dev/null || return 1

  jq -e -r '[.worlds[] | select(.enabled == true) | .name] | if length > 0 then sort[] else error("no enabled worlds") end' <<<"$json"
}
