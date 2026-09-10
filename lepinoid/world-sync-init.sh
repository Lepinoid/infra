#!/bin/sh
set -eu

BB="${BB:-/bin/busybox}"
ORAS_BIN="${ORAS_BIN:-oras}"
for _a in awk basename cat cp cut date df dirname find grep head mkdir mktemp mv rm sed sha256sum sort stat sync tar timeout tr wc; do
  eval "$_a() { \"\$BB\" $_a \"\$@\"; }"
done
unset _a

PHASE=pre
RESULT=no-op
DATA_DIR=${DATA_DIR:-/data}
WORK_DIR=${WORK_DIR:-/work}
PULL_TIMEOUT_SECONDS=${PULL_TIMEOUT_SECONDS:-900}
SWITCHED=
BACKED_UP=
ACTIVE=
TAB=$(printf '\t')
log() { printf '%s phase=%s %s\n' "$1" "$PHASE" "$2"; }
fail() { log ERROR "$*"; exit 1; }
hash() { sha256sum "$1" > "$WORK_DIR/hash"; cut -d ' ' -f 1 "$WORK_DIR/hash"; }

on_exit() {
  rc=$?
  trap - EXIT
  set +e
  if [ "$PHASE" = done ] && [ "$rc" -eq 0 ]; then
    printf 'RESULT=%s%s\n' "$RESULT" "${DETAIL:-}"
    exit 0
  fi
  log ERROR "operation failed rc=$rc; server startup will continue"
  if [ "$PHASE" = switch ]; then
    log ERROR "best-effort restore active=$ACTIVE switched=$SWITCHED moved-aside=$BACKED_UP"
    if [ -n "$ACTIVE" ] && [ -d "$WORK_DIR/old-$ACTIVE" ]; then
      # Never remove a blocker that we did not install (including regular files).
      case " $SWITCHED " in *" $ACTIVE "*) rm -rf "$DATA_DIR/$ACTIVE" ;; esac
      if mv "$WORK_DIR/old-$ACTIVE" "$DATA_DIR/$ACTIVE"; then
        log INFO "restored $ACTIVE"
      else
        log ERROR "restore failed for $ACTIVE; manual recovery required"
      fi
    fi
    log ERROR "completed worlds without retained old trees cannot be restored"
    RESULT=post-switch-failure
  else
    RESULT=pre-switch-failure
  fi
  for world in "$DATA_DIR"/*; do
    [ -d "$world" ] || continue
    if [ -f "$world/level.dat" ]; then
      log INFO "integrity: $world/level.dat present"
    else
      log ERROR "integrity: $world/level.dat missing"
    fi
  done
  rm -rf "$WORK_DIR/pull"
  printf 'RESULT=%s\n' "$RESULT"
  exit 0
}
trap on_exit EXIT

fetch_verify() {
  log INFO 'fetch and verify OCI manifest digest chain'
  timeout "$PULL_TIMEOUT_SECONDS" "$ORAS_BIN" manifest fetch --registry-config "$REGISTRY_CONFIG" \
    "$WORLD_BUNDLE_REPOSITORY@$D" > "$WORK_DIR/oci-manifest.json"
  [ "$(hash "$WORK_DIR/oci-manifest.json")" = "${D#sha256:}" ] || fail 'OCI manifest digest mismatch'
  tr -d ' \t\r\n' < "$WORK_DIR/oci-manifest.json" > "$WORK_DIR/oci-compact.json"
  grep -q '"artifactType":"application/vnd.lepinoid.world-bundle.v1"' "$WORK_DIR/oci-compact.json" || fail 'wrong artifactType'
  grep -o '"digest":"sha256:[0-9a-f]\{64\}"' "$WORK_DIR/oci-compact.json" |
    sed 's/.*sha256://;s/"$//' > "$WORK_DIR/allowed-digests"
  grep -o '"org.opencontainers.image.title":"[^"\\]*"' "$WORK_DIR/oci-compact.json" |
    sed 's/^[^:]*:"//;s/"$//' > "$WORK_DIR/titles"
  log INFO 'pull bundle'
  rm -rf "$WORK_DIR/pull"
  timeout "$PULL_TIMEOUT_SECONDS" "$ORAS_BIN" pull --registry-config "$REGISTRY_CONFIG" \
    -o "$WORK_DIR/pull" "$WORLD_BUNDLE_REPOSITORY@$D"
  find "$WORK_DIR/pull" -mindepth 1 -maxdepth 1 > "$WORK_DIR/pulled-files"
  while IFS= read -r file; do
    [ -f "$file" ] && [ ! -L "$file" ] || fail "non-regular pulled file: $file"
    grep -qxF "$(hash "$file")" "$WORK_DIR/allowed-digests" || fail "untrusted blob: $file"
  done < "$WORK_DIR/pulled-files"
}

parse_manifest() {
  log INFO 'parse bundle manifest'
  tr -d ' \t\r\n' < "$WORK_DIR/pull/manifest.json" > "$WORK_DIR/manifest-compact.json"
  # Fixed flat objects: validate every key/value, not just matching substrings.
  awk '
    function fields(s, a, n, i, p, k) {
      for (k in a) delete a[k]
      n=split(s,p,",")
      for(i=1;i<=n;i++) {
        k=p[i]; sub(/:.*/,"",k)
        if(k !~ /^"[A-Za-z][A-Za-z0-9]*"$/ || k in a) exit 1
        a[k]=substr(p[i],length(k)+2)
      }
      return n
    }
    function text(v) { if(v !~ /^"[^"\\]*"$/) exit 1; return substr(v,2,length(v)-2) }
    {
      if ($0 !~ /^\{.*,"worlds":\[\{.*\}\]\}$/) exit 1
      h=$0; sub(/,"worlds":.*/,"",h); sub(/^\{/,"",h)
      if(fields(h,a)!=4 || a["\"schemaVersion\""]!="1") exit 1
      git=text(a["\"gitCommitSha\""]); version=text(a["\"version\""])
      if(length(git)!=40 || git ~ /[^0-9a-f]/ || version !~ /^[0-9][0-9][0-9][0-9]\.[0-9][0-9]\.[0-9][0-9]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]$/) exit 1
      if(text(a["\"createdAt\""])=="") exit 1
      w=$0; sub(/^.*,"worlds":\[\{/,"",w); sub(/\}\]\}$/, "", w)
      count=split(w,objects,/\},\{/)
      for(j=1;j<=count;j++) {
        if(fields(objects[j],a)!=4) exit 1
        name=text(a["\"name\""]); path=text(a["\"archivePath\""])
        sha=text(a["\"sha256\""]); size=a["\"sizeBytes\""]
        if(name !~ /^[A-Za-z0-9][A-Za-z0-9._-]*$/ || length(name)>64 || seen[name]++ || path!=name ".tar.gz") exit 1
        if(length(sha)!=64 || sha ~ /[^0-9a-f]/ || size !~ /^[1-9][0-9]*$/) exit 1
        printf "%s\t%s\t%s\t%s\n",name,path,sha,size
      }
    }
    END { if(NR!=1) exit 1 }
  ' "$WORK_DIR/manifest-compact.json" > "$WORK_DIR/manifest.tsv" || fail 'invalid bundle manifest'
  gitCommitSha=$(sed 's/.*"gitCommitSha":"\([0-9a-f]*\)".*/\1/' "$WORK_DIR/manifest-compact.json")
  version=$(sed 's/.*"version":"\([^"]*\)".*/\1/' "$WORK_DIR/manifest-compact.json")
  { printf 'manifest.json\n'; cut -f 2 "$WORK_DIR/manifest.tsv"; } | sort > "$WORK_DIR/expected"
  while IFS= read -r file; do basename "$file"; done < "$WORK_DIR/pulled-files" | sort > "$WORK_DIR/actual"
  sort "$WORK_DIR/titles" > "$WORK_DIR/sorted-titles"
  [ "$(cat "$WORK_DIR/expected")" = "$(cat "$WORK_DIR/actual")" ] || fail 'unexpected pulled files'
  [ "$(cat "$WORK_DIR/expected")" = "$(cat "$WORK_DIR/sorted-titles")" ] || fail 'unexpected OCI titles'
}

check_space() {
  log INFO 'check free space'
  df -Pk "$DATA_DIR" > "$WORK_DIR/df-data"
  df -Pk "$WORK_DIR" > "$WORK_DIR/df-work"
  data_kb=$(awk 'END {print $4}' "$WORK_DIR/df-data")
  work_kb=$(awk 'END {print $4}' "$WORK_DIR/df-work")
  awk -v d="$data_kb" -v w="$work_kb" '
    {sum+=$4} END {
      printf "INFO space needed data=%.0fKB work=%.0fKB available data=%sKB work=%sKB\n",sum*2/1024,sum/1024,d,w
      if(d<sum*2/1024 || w<sum/1024) exit 1
    }' "$WORK_DIR/manifest.tsv" || fail 'insufficient free space'
}

verify_stage() {
  log INFO 'verify and stage all archives before switching'
  rm -rf "$DATA_DIR/.worlds-staging/$D"
  while IFS="$TAB" read -r name archive sha size; do
    log INFO "stage $name"
    file="$WORK_DIR/pull/$archive"
    [ "$(hash "$file")" = "$sha" ] || fail "archive checksum mismatch: $name"
    [ "$(stat -c %s "$file")" = "$size" ] || fail "archive size mismatch: $name"
    tar -tzvf "$file" > "$WORK_DIR/member-types"
    awk 'substr($0,1,1)!="d" && substr($0,1,1)!="-" {exit 1} / -> / {exit 1}' \
      "$WORK_DIR/member-types" || fail "unsafe archive member type: $name"
    tar -tzf "$file" > "$WORK_DIR/member-paths" 2> "$WORK_DIR/member-warnings"
    # BusyBox strips unsafe prefixes even when listing; reject its warnings.
    [ ! -s "$WORK_DIR/member-warnings" ] || fail "unsafe archive member path: $name"
    awk '!length || /^\// || /(^|\/)\.\.(\/|$)/ || /[[:cntrl:]]/ {exit 1}' \
      "$WORK_DIR/member-paths" || fail "unsafe archive member path: $name"
    mkdir -p "$DATA_DIR/.worlds-staging/$D/$name"
    tar -xzf "$file" -C "$DATA_DIR/.worlds-staging/$D/$name" --no-same-owner --no-same-permissions
    [ -f "$DATA_DIR/.worlds-staging/$D/$name/level.dat" ] || fail "missing level.dat: $name"
  done < "$WORK_DIR/manifest.tsv"
}

switch_all() {
  log INFO 'switch staged worlds'
  PHASE=switch
  while IFS="$TAB" read -r name archive sha size; do
    ACTIVE=$name
    log INFO "switch $name"
    if [ -d "$DATA_DIR/$name" ]; then
      [ ! -e "$WORK_DIR/old-$name" ] || fail "move-aside target already exists: $name"
      mv "$DATA_DIR/$name" "$WORK_DIR/old-$name"
      BACKED_UP="$BACKED_UP $name"
    fi
    mv "$DATA_DIR/.worlds-staging/$D/$name" "$DATA_DIR/$name"
    SWITCHED="$SWITCHED $name"
    rm -rf "$WORK_DIR/old-$name"
    ACTIVE=
  done < "$WORK_DIR/manifest.tsv"
  log INFO 'write current marker atomically'
  mkdir -p "$DATA_DIR/.worlds"
  printf '{"digest":"%s","gitCommitSha":"%s","version":"%s","appliedAt":"%s"}\n' \
    "$D" "$gitCommitSha" "$version" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DATA_DIR/.worlds/current.tmp.$$"
  mv "$DATA_DIR/.worlds/current.tmp.$$" "$DATA_DIR/.worlds/current"
  log INFO 'garbage collect temporary trees and sync'
  rm -rf "$DATA_DIR/.worlds-staging" "$WORK_DIR/pull" "$WORK_DIR"/old-*
  sync
  RESULT=switched
  DETAIL=" digest=$D version=$version"
  PHASE=done
}

log INFO 'validate environment'
[ -n "${WORLD_BUNDLE_REPOSITORY:-}" ] || fail 'missing WORLD_BUNDLE_REPOSITORY'
[ -d "$DATA_DIR" ] || fail 'DATA_DIR does not exist'
mkdir -p "$WORK_DIR"
[ -f "${REGISTRY_CONFIG:-}" ] || fail 'cannot authenticate: REGISTRY_CONFIG missing'
D=${WORLD_BUNDLE_DIGEST:-}
if [ -z "$D" ]; then log INFO 'no desired digest'; PHASE=done; exit 0; fi
printf '%s\n' "$D" | grep -q '^sha256:[0-9a-f]\{64\}$' || fail 'invalid digest'
log INFO 'check current marker'
if [ -f "$DATA_DIR/.worlds/current" ]; then
  current=$(sed -n 's/.*"digest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/.worlds/current")
  if [ "$current" = "$D" ]; then PHASE=done; exit 0; fi
fi
mkdir -p "$DATA_DIR/.worlds-staging"
fetch_verify
parse_manifest
check_space
verify_stage
switch_all
