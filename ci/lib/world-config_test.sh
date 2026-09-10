#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source ./world-config.sh

fixture_dir=$(mktemp -d)
trap 'rm -rf "$fixture_dir"' EXIT

cat >"$fixture_dir/valid.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
# comment before data
data:
  # comment inside data
  config.json: |-
    {"schemaVersion":1,"worlds":[{"name":"z-world","enabled":true},{"name":"a.world","enabled":false},{"name":"m_world","enabled":true}]}
other: value
YAML

expected='{"schemaVersion":1,"worlds":[{"name":"z-world","enabled":true},{"name":"a.world","enabled":false},{"name":"m_world","enabled":true}]}'
[[ "$(world_config_extract_json "$fixture_dir/valid.yaml")" == "$expected" ]]
[[ "$(world_config_enabled_names "$fixture_dir/valid.yaml")" == $'m_world\nz-world' ]]

cat >"$fixture_dir/missing.yaml" <<'YAML'
  other: |-
    {}
YAML
if world_config_enabled_names "$fixture_dir/missing.yaml"; then exit 1; fi

write_json() {
  local name=$1 json=$2
  {
    printf 'data:\n  config.json: |-\n    '
    printf '%s\n' "$json"
  } >"$fixture_dir/$name.yaml"
}

write_json schema-version '{"schemaVersion":2,"worlds":[{"name":"world","enabled":true}]}'
if world_config_enabled_names "$fixture_dir/schema-version.yaml"; then exit 1; fi
write_json extra-top-level '{"schemaVersion":1,"worlds":[],"extra":true}'
if world_config_enabled_names "$fixture_dir/extra-top-level.yaml"; then exit 1; fi
write_json duplicate '{"schemaVersion":1,"worlds":[{"name":"world","enabled":true},{"name":"world","enabled":false}]}'
if world_config_enabled_names "$fixture_dir/duplicate.yaml"; then exit 1; fi
write_json bad-name-parent '{"schemaVersion":1,"worlds":[{"name":"../x","enabled":true}]}'
if world_config_enabled_names "$fixture_dir/bad-name-parent.yaml"; then exit 1; fi
write_json bad-name-space '{"schemaVersion":1,"worlds":[{"name":"a b","enabled":true}]}'
if world_config_enabled_names "$fixture_dir/bad-name-space.yaml"; then exit 1; fi
write_json bad-name-hidden '{"schemaVersion":1,"worlds":[{"name":".hidden","enabled":true}]}'
if world_config_enabled_names "$fixture_dir/bad-name-hidden.yaml"; then exit 1; fi
write_json bad-name-long "{\"schemaVersion\":1,\"worlds\":[{\"name\":\"$(printf 'a%.0s' {1..65})\",\"enabled\":true}]}"
if world_config_enabled_names "$fixture_dir/bad-name-long.yaml"; then exit 1; fi
write_json enabled-string '{"schemaVersion":1,"worlds":[{"name":"world","enabled":"true"}]}'
if world_config_enabled_names "$fixture_dir/enabled-string.yaml"; then exit 1; fi
write_json no-enabled '{"schemaVersion":1,"worlds":[{"name":"world","enabled":false}]}'
if world_config_enabled_names "$fixture_dir/no-enabled.yaml"; then exit 1; fi

echo "all world-config tests passed"
