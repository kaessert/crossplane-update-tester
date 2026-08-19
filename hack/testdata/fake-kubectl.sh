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
#   logs -n crossplane-system -l pkg.crossplane.io/revision --tail=-1 --since=<n>s
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
#                            "loop" — every `get events` poll bumps
#                                ev_update for every resource in state, with
#                                no atProvider drift at all, i.e. a
#                                reconciler that is genuinely stuck calling
#                                Update() repeatedly — the event-count half
#                                of what `converge` catches, independent of
#                                the atProvider-diff half "drift" exercises.
#                            "logloop" — the same stuck reconciler as "loop",
#                                but visible ONLY in the controller log:
#                                `get events` never moves, exactly as
#                                client-go's rate limiter behaves for a
#                                resource updating on every poll tick. This
#                                is the measured live failure the log
#                                instrument exists for, and the one every
#                                other instrument reports as stable.
#   SMOKE_UPPERCASE_FIELDS   space-separated forProvider field names the
#                            "backend" normalises to upper case when storing
#                            them, so a manifest entry's `expect:` differing
#                            from its `value:` is exercised.
#   SMOKE_WIPE_FIELD         name of an atProvider field the "backend"
#                            silently resets to SMOKE_WIPE_TO on every
#                            accepted spec patch, regardless of which field
#                            was actually patched — the exact defect an
#                            `assert-unchanged:` directive exists to catch
#                            (an omitted union member reset to its default on
#                            every write). Unset (the default): no wipe.
#   SMOKE_WIPE_TO            value SMOKE_WIPE_FIELD is reset to. Defaults to
#                            the empty string.
set -euo pipefail

STATE="${SMOKE_STATE:?fake kubectl: SMOKE_STATE is not set}"
FAIL_MODE="${SMOKE_FAIL_MODE:-}"
UPPERCASE_FIELDS="${SMOKE_UPPERCASE_FIELDS:-}"
WIPE_FIELD="${SMOKE_WIPE_FIELD:-}"
WIPE_TO="${SMOKE_WIPE_TO:-}"

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
# One directory per resource under $STATE/res/<res_key>/, holding:
#   name apiVersion kind namespace type generation extname extname_prev
#   paused ev_update ev_create drift
#   spec/<field>   the value the last merge patch put in spec.forProvider
#   at/<field>     the value the backend stores, i.e. status.atProvider
#
# res_key is keyed by NAMESPACE+NAME, not name alone: the unified
# example-manifest convention every dual-scope provider follows gives the
# cluster-scoped and namespaced variants of a Kind the exact same
# metadata.name, differing only in namespace and apiVersion group. Keying by
# name alone would collide the two into one state directory the moment a
# fixture actually exercises that convention.
res_key() {
  local namespace=$1 name=$2
  printf '%s__%s' "${namespace:-_cluster}" "$name"
}

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

  rd="$STATE/res/$(res_key "$namespace" "$name")"
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

# resource_dir maps a "<type>/<name>" identifier plus the namespace (empty
# for cluster-scoped, as captured by strip_namespace) back to its state
# directory. The namespace is REQUIRED, not inferred: two resources sharing
# a name across scopes are two different state directories (see res_key).
resource_dir() {
  local ident=$1 namespace=${2:-}
  local rd="$STATE/res/$(res_key "$namespace" "${ident##*/}")"
  [ -d "$rd" ] || die "no such resource: $ident (namespace='$namespace') (state dir $rd does not exist)"
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
# namespaced resource. The remaining args are left in ARGS, and the
# namespace value itself (empty if no "-n" was present) is left in NS — the
# resource-scoped commands need it to key into $STATE/res (see res_key), not
# just to strip it. It is applied only to the resource-scoped commands: `get
# pods` and `rollout` carry a -n of their own, which they are checked for.
ARGS=()
NS=""
strip_namespace() {
  ARGS=()
  NS=""
  local skip=0 i=1
  local -a in=("$@")
  for ((i = 0; i < ${#in[@]}; i++)); do
    if [ "$skip" = 1 ]; then
      skip=0
      continue
    fi
    if [ "${in[$i]}" = "-n" ]; then
      NS="${in[$((i + 1))]:-}"
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

    # FAIL_MODE=loop simulates a controller that is genuinely stuck calling
    # Update() repeatedly: every `get events` poll bumps ev_update for every
    # resource currently in state, so two successive polls (the baseline and
    # the outcome a converge check takes) see a growing count with no
    # backend field ever changing — the exact signature a real reconcile
    # loop produces, and the one converge exists to catch independently of
    # any atProvider drift.
    if [ "$FAIL_MODE" = "loop" ]; then
      local ldir
      for ldir in "$STATE"/res/*/; do
        [ -d "$ldir" ] || continue
        printf '%s' "$(($(cat "$ldir/ev_update") + 1))" >"$ldir/ev_update"
      done
    fi

    local items=() dir name kind namespace apiVersion u c
    for dir in "$STATE"/res/*/; do
      [ -d "$dir" ] || continue
      name=$(cat "$dir/name")
      kind=$(cat "$dir/kind")
      namespace=$(cat "$dir/namespace")
      apiVersion=$(cat "$dir/apiVersion")
      u=$(cat "$dir/ev_update")
      c=$(cat "$dir/ev_create")
      if [ "$u" -gt 0 ]; then
        items+=("{\"reason\":\"UpdatedExternalResource\",\"count\":$u,\"involvedObject\":{\"kind\":\"$(json_escape "$kind")\",\"name\":\"$(json_escape "$name")\",\"namespace\":\"$(json_escape "$namespace")\",\"apiVersion\":\"$(json_escape "$apiVersion")\"}}")
      fi
      if [ "$c" -gt 0 ]; then
        items+=("{\"reason\":\"CreatedExternalResource\",\"count\":$c,\"involvedObject\":{\"kind\":\"$(json_escape "$kind")\",\"name\":\"$(json_escape "$name")\",\"namespace\":\"$(json_escape "$namespace")\",\"apiVersion\":\"$(json_escape "$apiVersion")\"}}")
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
  rd=$(resource_dir "$1" "$NS")
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
  dir=$(resource_dir "$rd" "$NS")

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

      # SMOKE_WIPE_FIELD simulates a backend that silently resets an
      # unrelated field on every accepted write — independent of FAIL_MODE
      # and independent of which field was actually patched, mirroring a
      # backend bug that is not scoped to the field under test.
      if [ -n "$WIPE_FIELD" ]; then
        printf '%s' "$WIPE_TO" >"$dir/at/$WIPE_FIELD"
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
  resource_dir "$rd" "$NS" >/dev/null
  printf '%s condition met\n' "$rd"
}

cmd_logs() {
  # logs -n crossplane-system -l pkg.crossplane.io/revision --tail=-1 --since=<n>s
  case "$*" in
    *"-n crossplane-system"*) ;;
    *) die "logs not scoped to crossplane-system: $*" ;;
  esac
  case "$*" in
    *"-l pkg.crossplane.io/revision"*) ;;
    *) die "logs not selecting the provider revision: $*" ;;
  esac
  # --tail=-1 is asserted, not merely tolerated. kubectl defaults --tail to 10
  # whenever a selector is used, so a caller that omits it reads the last ten
  # lines instead of the window and under-counts silently. That is a contract
  # this fake exists to pin, not an implementation detail.
  case "$*" in
    *"--tail=-1"*) ;;
    *) die "logs must pass --tail=-1 with a selector, or kubectl silently truncates to 10 lines: $*" ;;
  esac

  # One benign reconcile line per resource in state: a controller that is
  # demonstrably alive and logging, having made no Update() call. "Alive and
  # quiet" must be distinguishable from "not logging at all", which is what a
  # provider running without --debug produces.
  local dir name namespace req
  for dir in "$STATE"/res/*/; do
    [ -d "$dir" ] || continue
    name=$(cat "$dir/name")
    namespace=$(cat "$dir/namespace")
    if [ -n "$namespace" ]; then
      req="{\"name\":\"$(json_escape "$name")\",\"namespace\":\"$(json_escape "$namespace")\"}"
    else
      req="{\"name\":\"$(json_escape "$name")\"}"
    fi
    printf '2026-01-01T00:00:00Z\tDEBUG\tprovider-fake\tReconciling\t{"request": %s}\n' "$req"
    # FAIL_MODE=logloop adds the Update() line the managed reconciler writes
    # on every call, while `get events` stays frozen — the live signature of
    # client-go's rate limiter holding an aggregated count still across a
    # resource that is updating on every poll tick.
    if [ "$FAIL_MODE" = "logloop" ]; then
      printf '2026-01-01T00:00:00Z\tDEBUG\tprovider-fake\tSuccessfully requested update of external resource\t{"request": %s, "version": "1"}\n' "$req"
    fi
  done
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
    logs) cmd_logs "$@" ;;
    *) die "unhandled subcommand: $verb $*" ;;
  esac
}

main "$@"
