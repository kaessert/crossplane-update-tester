#!/usr/bin/env bash
# fake-kubectl.sh — a stateful stand-in for kubectl, used by hack/smoke-test.sh.
#
# hack/smoke-test.sh copies this file to <tmp>/bin/kubectl and puts that
# directory first on PATH, so the update-tester binary shells out to this
# script exactly as it would to the real kubectl. It answers only the argv
# shapes the tool actually issues (determined empirically by running the tool
# against a logging-only version of this script and reading the log back):
#
#   get -f <ABS manifest> -o name
#   get <type>/<name> -o json                       [-n <ns>]
#   patch <type>/<name> --type=merge -p <json>      [-n <ns>]
#   patch <type>/<name> --subresource=status --type=merge -p <json>  [-n <ns>]
#   wait <type>/<name> --for=condition=Ready --timeout=<d>  [-n <ns>]
#   get events --all-namespaces -o json
#   get pods -n crossplane-system -l pkg.crossplane.io/revision -o jsonpath={range ...}
#   rollout restart deploy/<name> -n crossplane-system
#   rollout status  deploy/<name> -n crossplane-system --timeout=<d>
#
# Anything else is a hard error: an unhandled argv means the tool changed the
# contract this fake encodes, and silently returning success there would let a
# broken hook pass the smoke test.
#
# It models a trivially well-behaved backend: a spec.forProvider merge patch
# bumps metadata.generation, is mirrored into status.atProvider, and emits one
# UpdatedExternalResource event. status.conditions always carries
# observedGeneration == metadata.generation, so the resource "settles"
# immediately.
#
# Environment:
#   SMOKE_STATE              (required) directory holding this fake's state +
#                            the invocation log (kubectl.log).
#   SMOKE_FAIL_MODE          "" (well-behaved, default)
#                            "drift" — status.atProvider grows a field that
#                                changes on every read, i.e. the reconcile
#                                loop `converge` exists to catch.
#                            "stuck" — spec patches are accepted and events
#                                are emitted, but status.atProvider never
#                                picks the new value up, i.e. the silent
#                                update failure `run` exists to catch.
#   SMOKE_UPPERCASE_FIELDS   space-separated forProvider field names the
#                            "backend" normalises to upper case when storing
#                            them, so a manifest entry's `expect:` differing
#                            from its `value:` is exercised.
set -euo pipefail

STATE="${SMOKE_STATE:?fake kubectl: SMOKE_STATE is not set}"
FAIL_MODE="${SMOKE_FAIL_MODE:-}"
UPPERCASE_FIELDS="${SMOKE_UPPERCASE_FIELDS:-}"

mkdir -p "$STATE/res"
LOG="$STATE/kubectl.log"

# ─── logging / errors ──────────────────────────────────────────────────────

printf '%s\n' "$*" >>"$LOG"

die() {
  printf 'FAKE-KUBECTL-ERROR: %s\n' "$*" >>"$LOG"
  printf 'fake kubectl: %s\n' "$*" >&2
  exit 1
}

# json_escape escapes a value for embedding in a JSON string literal. The
# fixtures only use printable ASCII, so backslash and double quote are the
# only two characters that need it.
json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# ─── resource state ────────────────────────────────────────────────────────
#
# One directory per resource under $STATE/res/<metadata.name>/, holding:
#   name apiVersion kind namespace type generation extname extname_prev
#   paused ev_update ev_create drift
#   spec/<field>   the value the last merge patch put in spec.forProvider
#   at/<field>     the value the backend stores, i.e. status.atProvider

# seed_from_manifest reads a manifest and materialises the resource state it
# describes, if it does not exist yet. This is what makes the fake generic
# over the fixtures: the "backend" starts out holding exactly what the
# example asks for.
seed_from_manifest() {
  local f=$1 rd
  local apiVersion kind name namespace prefix plural

  apiVersion=$(awk '/^apiVersion:/{sub(/^apiVersion:[ \t]*/,"");print;exit}' "$f")
  kind=$(awk '/^kind:/{sub(/^kind:[ \t]*/,"");print;exit}' "$f")
  name=$(awk '/^  name:/{sub(/^  name:[ \t]*/,"");print;exit}' "$f")
  namespace=$(awk '/^  namespace:/{sub(/^  namespace:[ \t]*/,"");print;exit}' "$f")
  prefix=$(awk -F': *' '/crossplane.io\/expect-external-name-prefix:/{print $2;exit}' "$f" | tr -d '"')

  [ -n "$apiVersion" ] || die "manifest $f has no apiVersion"
  [ -n "$kind" ] || die "manifest $f has no kind"
  [ -n "$name" ] || die "manifest $f has no metadata.name"

  rd="$STATE/res/$name"
  if [ -d "$rd" ]; then
    printf '%s' "$rd"
    return
  fi

  mkdir -p "$rd/spec" "$rd/at"
  printf '%s' "$name" >"$rd/name"
  printf '%s' "$kind" >"$rd/kind"
  printf '%s' "$apiVersion" >"$rd/apiVersion"
  printf '%s' "$namespace" >"$rd/namespace"
  # kubectl renders `get -o name` as <plural>.<group>/<name>.
  plural="$(printf '%s' "$kind" | tr '[:upper:]' '[:lower:]')s"
  printf '%s/%s' "$plural.${apiVersion%%/*}" "$name" >"$rd/type"
  printf '1' >"$rd/generation"
  printf '0' >"$rd/ev_update"
  # The resource is created once before any post-assert hook runs, so its
  # lifecycle already carries exactly one CreatedExternalResource event.
  printf '1' >"$rd/ev_create"
  printf '0' >"$rd/paused"
  # The backend assigns an identity on Create. For a kind whose backend
  # models several object types, that identity is prefixed with the object
  # type actually used — which is what the manifest declares it expects.
  printf '%s%s' "$prefix" "$name" >"$rd/extname"

  # spec.forProvider — a flat block of "    key: value" lines. status.
  # atProvider starts out mirroring it, as it would after a successful
  # Create + Observe.
  awk '
    /^  forProvider:/ { inblock = 1; next }
    inblock && /^    [A-Za-z]/ { line = $0; sub(/^    /, "", line); print line; next }
    inblock { exit }
  ' "$f" | while IFS= read -r line; do
    local k v
    k=${line%%:*}
    v=${line#*: }
    v=${v%\"}
    v=${v#\"}
    printf '%s' "$v" >"$rd/spec/$k"
    printf '%s' "$v" >"$rd/at/$k"
  done

  printf '%s' "$rd"
}

# resource_dir maps a "<type>/<name>" identifier back to its state directory.
resource_dir() {
  local rd="$STATE/res/${1##*/}"
  [ -d "$rd" ] || die "no such resource: $1 (state dir $rd does not exist)"
  printf '%s' "$rd"
}

# render_fields renders a directory of field files as a JSON object body.
render_fields() {
  local dir=$1 out="" sep="" f k v
  for f in "$dir"/*; do
    [ -e "$f" ] || continue
    k=$(basename "$f")
    v=$(cat "$f")
    out="$out$sep\"$(json_escape "$k")\":\"$(json_escape "$v")\""
    sep=","
  done
  printf '%s' "$out"
}

# render_at_provider renders status.atProvider. In drift mode it grows an
# extra observed field whose value is read from the wall clock, so any two
# reads taken at different instants disagree — a resource that never holds
# still, which is precisely what `converge` asserts against.
#
# The value is keyed off TIME rather than a per-read counter. Both detect
# drift equally well today; time is preferred because it models the thing
# being faked (a backend field that advances on a clock, e.g. a last-sync
# timestamp) rather than the observer, and because it needs no mutable state
# file to reset between scenarios. A counter makes the rendered value a
# function of how many times the tool happened to look, which is a property
# no real backend has.
render_at_provider() {
  local rd=$1 body
  body=$(render_fields "$rd/at")
  if [ "$FAIL_MODE" = "drift" ]; then
    [ -n "$body" ] && body="$body,"
    body="$body\"lastObservedAt\":\"$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)\""
  fi
  printf '{%s}' "$body"
}

render_annotations() {
  local rd=$1 out="" sep="" extname
  extname=$(cat "$rd/extname")
  if [ -n "$extname" ]; then
    out="$out$sep\"crossplane.io/external-name\":\"$(json_escape "$extname")\""
    sep=","
  fi
  if [ "$(cat "$rd/paused")" = "1" ]; then
    out="$out$sep\"crossplane.io/paused\":\"true\""
    sep=","
  fi
  printf '%s' "$out"
}

# render_object renders the whole resource as `kubectl get -o json` would.
# status.conditions always carries observedGeneration == metadata.generation:
# this fake's "controller" reconciles instantly.
render_object() {
  local rd=$1 gen
  gen=$(cat "$rd/generation")
  printf '{'
  printf '"apiVersion":"%s","kind":"%s",' "$(cat "$rd/apiVersion")" "$(cat "$rd/kind")"
  printf '"metadata":{"name":"%s","namespace":"%s","generation":%s,"annotations":{%s}},' \
    "$(cat "$rd/name")" "$(cat "$rd/namespace")" "$gen" "$(render_annotations "$rd")"
  printf '"spec":{"forProvider":{%s}},' "$(render_fields "$rd/spec")"
  printf '"status":{"atProvider":%s,"conditions":[{"type":"Ready","status":"True","reason":"Available","observedGeneration":%s},{"type":"Synced","status":"True","reason":"ReconcileSuccess","observedGeneration":%s}]}' \
    "$(render_at_provider "$rd")" "$gen" "$gen"
  printf '}\n'
}

# ─── argv dispatch ─────────────────────────────────────────────────────────

# strip_namespace removes a "-n <ns>" pair, which Runner.run() appends for a
# namespaced resource. The remaining args are left in ARGS. It is applied only
# to the resource-scoped commands: `get pods` and `rollout` carry a -n of
# their own, which they are checked for.
ARGS=()
strip_namespace() {
  ARGS=()
  local skip=0 i=1
  local -a in=("$@")
  for ((i = 0; i < ${#in[@]}; i++)); do
    if [ "$skip" = 1 ]; then
      skip=0
      continue
    fi
    if [ "${in[$i]}" = "-n" ]; then
      skip=1
      continue
    fi
    ARGS+=("${in[$i]}")
  done
}

cmd_get() {
  local rd

  # get -f <manifest> -o name
  if [ "${1:-}" = "-f" ]; then
    local manifest=$2
    # Edge 1: `go -C tools/update-tester` runs the tool with a different
    # working directory, so a relative manifest path either misses or finds
    # the wrong file. This fake refuses to resolve one at all.
    case "$manifest" in
      /*) ;;
      *) die "manifest path is not absolute: '$manifest' (cwd=$PWD) — a relative path cannot survive 'go -C'" ;;
    esac
    [ -f "$manifest" ] || die "manifest does not exist: $manifest"
    [ "${3:-}" = "-o" ] && [ "${4:-}" = "name" ] || die "unhandled 'get -f' argv: $*"
    rd=$(seed_from_manifest "$manifest")
    cat "$rd/type"
    printf '\n'
    return
  fi

  # get events --all-namespaces -o json
  if [ "${1:-}" = "events" ]; then
    [ "${2:-}" = "--all-namespaces" ] && [ "${3:-}" = "-o" ] && [ "${4:-}" = "json" ] ||
      die "unhandled 'get events' argv: $*"
    local items=() dir name kind u c
    for dir in "$STATE"/res/*/; do
      [ -d "$dir" ] || continue
      name=$(cat "$dir/name")
      kind=$(cat "$dir/kind")
      u=$(cat "$dir/ev_update")
      c=$(cat "$dir/ev_create")
      if [ "$u" -gt 0 ]; then
        items+=("{\"reason\":\"UpdatedExternalResource\",\"count\":$u,\"involvedObject\":{\"kind\":\"$kind\",\"name\":\"$name\"}}")
      fi
      if [ "$c" -gt 0 ]; then
        items+=("{\"reason\":\"CreatedExternalResource\",\"count\":$c,\"involvedObject\":{\"kind\":\"$kind\",\"name\":\"$name\"}}")
      fi
    done
    local joined=""
    if [ ${#items[@]} -gt 0 ]; then
      joined=$(
        IFS=,
        printf '%s' "${items[*]}"
      )
    fi
    printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$joined"
    return
  fi

  # get pods -n crossplane-system -l pkg.crossplane.io/revision -o jsonpath=...
  if [ "${1:-}" = "pods" ]; then
    case "$*" in
      *"-n crossplane-system"*"pkg.crossplane.io/revision"*) ;;
      *) die "unhandled 'get pods' argv: $*" ;;
    esac
    # One installed package revision → one unambiguous Deployment name.
    printf 'provider-example-7f4c2b91d0ac\n'
    return
  fi

  # get <type>/<name> [-n <ns>] -o json
  #
  # The resource is only ever read as a whole object: the tool navigates to
  # status.atProvider and renders it in Go rather than asking kubectl to
  # print that subtree, so nothing depends on how a given kubectl version
  # renders a non-scalar value. Any other output spec is therefore an
  # unhandled argv, not a shape to emulate.
  strip_namespace "$@"
  set -- "${ARGS[@]+"${ARGS[@]}"}"
  rd=$(resource_dir "$1")
  [ "${2:-}" = "-o" ] || die "unhandled 'get' argv: $*"
  case "${3:-}" in
    json)
      render_object "$rd"
      ;;
    *)
      die "unhandled 'get' output spec: $*"
      ;;
  esac
}

cmd_patch() {
  strip_namespace "$@"
  set -- "${ARGS[@]+"${ARGS[@]}"}"
  local rd=$1
  shift
  local subresource="" patch="" i
  local -a rest=("$@")
  for ((i = 0; i < ${#rest[@]}; i++)); do
    case "${rest[$i]}" in
      --subresource=status) subresource="status" ;;
      --type=merge) ;;
      -p)
        i=$((i + 1))
        patch="${rest[$i]}"
        ;;
      *) die "unhandled 'patch' flag: ${rest[$i]}" ;;
    esac
  done
  [ -n "$patch" ] || die "'patch' called without -p"

  local dir
  dir=$(resource_dir "$rd")

  # A status-subresource patch (clearing conditions) is a no-op here: this
  # fake's controller re-establishes Ready with the current observedGeneration
  # on the next read, which is exactly what ClearConditions + WaitReady
  # assumes.
  if [ "$subresource" = "status" ]; then
    printf '%s patched\n' "$rd"
    return
  fi

  case "$patch" in
    '{"spec":{"forProvider":{'*)
      local inner field value
      inner=${patch#'{"spec":{"forProvider":{'}
      inner=${inner%'}}}'}
      field=${inner%%\":*}
      field=${field#\"}
      value=${inner#*:}
      value=${value#\"}
      value=${value%\"}
      [ -n "$field" ] || die "could not parse field out of patch: $patch"

      printf '%s' "$value" >"$dir/spec/$field"
      # A spec change bumps metadata.generation, and the reconcile it
      # triggers calls Update() — one UpdatedExternalResource event.
      printf '%s' "$(($(cat "$dir/generation") + 1))" >"$dir/generation"
      printf '%s' "$(($(cat "$dir/ev_update") + 1))" >"$dir/ev_update"

      if [ "$FAIL_MODE" != "stuck" ]; then
        local stored=$value w
        for w in $UPPERCASE_FIELDS; do
          if [ "$w" = "$field" ]; then
            stored=$(printf '%s' "$value" | tr '[:lower:]' '[:upper:]')
          fi
        done
        printf '%s' "$stored" >"$dir/at/$field"
      fi
      ;;

    '{"metadata":{"annotations":{'*)
      local inner key value
      inner=${patch#'{"metadata":{"annotations":{'}
      inner=${inner%'}}}'}
      key=${inner%%\":*}
      key=${key#\"}
      value=${inner#*:}
      value=${value#\"}
      value=${value%\"}
      case "$key" in
        crossplane.io/paused)
          if [ "$value" = "null" ]; then
            printf '0' >"$dir/paused"
            # Unpausing is what triggers the recovery reconcile. With no
            # external-name to go on, the controller resolves identity by
            # searching the backend — and finds the object it created
            # earlier, so no second CreatedExternalResource event.
            if [ -z "$(cat "$dir/extname")" ] && [ -f "$dir/extname_prev" ]; then
              cp "$dir/extname_prev" "$dir/extname"
            fi
          else
            printf '1' >"$dir/paused"
          fi
          ;;
        crossplane.io/external-name)
          [ "$value" = "null" ] || die "unexpected external-name patch value: $value"
          cp "$dir/extname" "$dir/extname_prev"
          : >"$dir/extname"
          ;;
        update-tester.crossplane.io/nudge)
          # An annotation-only write: it wakes the controller but changes
          # neither spec nor metadata.generation.
          ;;
        *) die "unhandled annotation patch key: $key" ;;
      esac
      ;;

    *)
      die "unhandled patch body: $patch"
      ;;
  esac

  printf '%s patched\n' "$rd"
}

cmd_wait() {
  strip_namespace "$@"
  set -- "${ARGS[@]+"${ARGS[@]}"}"
  local rd=$1
  shift
  case "$*" in
    "--for=condition=Ready --timeout="*) ;;
    *) die "unhandled 'wait' argv: $rd $*" ;;
  esac
  resource_dir "$rd" >/dev/null
  printf '%s condition met\n' "$rd"
}

cmd_rollout() {
  local action=$1 target=$2
  case "$target" in
    deploy/*) ;;
    *) die "unhandled rollout target: $target" ;;
  esac
  case "$*" in
    *"-n crossplane-system"*) ;;
    *) die "rollout not scoped to crossplane-system: $*" ;;
  esac
  case "$action" in
    restart)
      printf '%s\n' "$target" >>"$STATE/rollout-restarts"
      printf 'deployment.apps/%s restarted\n' "${target#deploy/}"
      ;;
    status)
      printf 'deployment "%s" successfully rolled out\n' "${target#deploy/}"
      ;;
    *) die "unhandled rollout action: $action" ;;
  esac
}

main() {
  [ $# -gt 0 ] || die "called with no arguments"
  local verb=$1
  shift

  case "$verb" in
    get) cmd_get "$@" ;;
    patch) cmd_patch "$@" ;;
    wait) cmd_wait "$@" ;;
    rollout) cmd_rollout "$@" ;;
    *) die "unhandled subcommand: $verb $*" ;;
  esac
}

main "$@"
