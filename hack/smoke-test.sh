#!/usr/bin/env bash
# smoke-test.sh — end-to-end proof of the consumption pattern this tool
# documents, with no cluster and no published module.
#
# What it proves, and why each one is worth a test:
#
#  1. A stub module holding ONLY a go.mod (no Go files of its own) really can
#     host and execute this tool via `go -C <stub> tool crossplane-update-tester`.
#  2. Invoking a post-assert SYMLINK runs the full 5-step sequence in order.
#     `$0` is the symlink's own name — bash does not resolve symlinks before
#     `basename` — which is the entire mechanism by which one wrapper script
#     serves every resource. If it ever resolved, every symlink would derive
#     the manifest `run-update-tester.yaml` and nothing would work.
#  3. A manifest WITHOUT crossplane.io/expect-external-name-prefix runs the
#     3-step sequence, so the two identity checks really are gated on the
#     annotation rather than always-on.
#  4. The manifest path that reaches the tool is ABSOLUTE and resolves to the
#     right file per symlink. `go -C` changes the child's working directory,
#     so a relative path either misses or — worse — finds a different file.
#  5. Invoking a post-assert hook the way `uptest` actually does — a path
#     relative to the example's own directory — resolves to the same
#     absolute manifest a direct invocation does.
#  6. Two managed resources sharing the same Kind and the same name, differing
#     only in scope, never have their update events attributed to each other.
#
# It builds a throwaway fake provider tree under $TMPDIR and runs the real
# binary against an in-process fake Kubernetes API server
# (hack/faketestserver), reached through an ordinary kubeconfig — no real
# cluster, no exec-forced kubectl transcript. The hook wrapper is extracted
# from README.md at run time rather than copied here, so this test validates
# the documented script and fails if the README drifts away from it.
#
# This exercises the tool's DEFAULT client-go backend, end to end, through the
# real compiled binary, invoked via a real post-assert symlink, in a stub
# provider tree, against real example manifests and annotations — the same
# consumption pattern a provider's own E2E run relies on.
#
# The failure-injection scenarios this harness used to drive through a
# stateful fake kubectl (drift, a stuck update, a reconciliation loop visible
# only via the event count or only via the controller log, and the
# assert-unchanged silent-wipe guard) are internal/runner unit tests now: they
# exercise this tool's own evidence logic on a Go value, not the shape of a
# request on a wire, and are cheaper and more precise to assert there. See
# internal/runner's own *_test.go files.
#
# Requirements: go, bash, coreutils. No cluster, no network (the module cache
# is already warm if this repo builds).
#
# Usage: bash hack/smoke-test.sh    (from any working directory)
#        SMOKE_KEEP=1 bash hack/smoke-test.sh   keeps the tree, the fake
#        server's log and every hook transcript for inspection instead of
#        deleting them on exit.

set -euo pipefail

# ─── setup ─────────────────────────────────────────────────────────────────

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TESTDATA="$REPO_ROOT/hack/testdata"
MODULE_PATH="github.com/kaessert/crossplane-update-tester"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/crossplane-update-tester-smoke.XXXXXX")"
TREE="$TMP/fake-provider"
STUB="$TREE/tools/update-tester"
BIN="$TMP/bin"
ELSEWHERE="$TMP/elsewhere"

FAKESERVER_PID=""

cleanup() {
  # Tear the fake server down on every exit path, including a failure that
  # exits early via `abort` — a leaked listener across CI runs is a flake
  # nobody will reproduce locally.
  if [ -n "$FAKESERVER_PID" ] && kill -0 "$FAKESERVER_PID" 2>/dev/null; then
    kill "$FAKESERVER_PID" 2>/dev/null || true
    wait "$FAKESERVER_PID" 2>/dev/null || true
  fi
  if [ -n "${SMOKE_KEEP:-}" ]; then
    printf '\nSMOKE_KEEP set — leaving the tree and transcripts at %s\n' "$TMP"
    return
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

FAILURES=0
CHECKS=0

section() { printf '\n== %s\n' "$*"; }
ok() {
  CHECKS=$((CHECKS + 1))
  printf '   ok   %s\n' "$*"
}
bad() {
  CHECKS=$((CHECKS + 1))
  FAILURES=$((FAILURES + 1))
  printf '   FAIL %s\n' "$*" >&2
}
# abort is for setup problems: nothing downstream can mean anything, so stop.
abort() {
  printf '\nFAIL (setup): %s\n' "$*" >&2
  exit 1
}

# dump prints a captured transcript when a check fails, so the failure message
# is actionable without re-running by hand. The temp tree is gone by then.
dump() {
  local label=$1 file=$2
  printf '   ---- %s ----\n' "$label" >&2
  sed 's/^/   | /' "$file" >&2
  printf '   ----\n' >&2
}

command -v go >/dev/null || abort "go is not on PATH"

# ─── build the fake provider tree ──────────────────────────────────────────

section "building fake provider tree at $TREE"

mkdir -p "$STUB" "$TREE/test/hooks" "$BIN" "$ELSEWHERE"
cp -R "$TESTDATA/examples" "$TREE/examples"

# The stub module: exactly the shape README.md documents for a consumer, with
# one deliberate difference — a `replace` pointing at this checkout.
#
# That replace is PERMANENT, not a pre-publication workaround. A consumer pins
# a released version; this test must exercise the working tree, because its job
# is to gate the code in front of it. Resolving the tool from the module proxy
# here would mean a change that breaks the consumption pattern still passes its
# own CI, and the breakage only surfaces in a provider's E2E run after release
# — exactly the failure this test exists to prevent. Do not "modernise" this
# into `go get -tool <module>@vX.Y.Z`.
{
  echo "module github.com/example/provider-example/tools/update-tester"
  echo
  # Derived from the repo's own go.mod — the same single source of truth the
  # CI workflow uses via `go-version-file: go.mod`. A hard-coded literal here
  # drifts every time the tool's own go directive is raised.
  grep -E '^go [0-9]' "$REPO_ROOT/go.mod"
  echo
  echo "tool $MODULE_PATH"
  echo
  echo "require $MODULE_PATH v0.0.0"
  echo
  # The tool's own requirements have to be restated here, because with a
  # directory `replace` there is no module zip for the go command to read
  # them from lazily. This part IS an artefact of the local replace — a
  # consumer's `go get -tool` writes these automatically.
  awk '
    /^require \(/ { inblock = 1; next }
    inblock && /^\)/ { inblock = 0; next }
    inblock { print "require " $1 " " $2; next }
    /^require [^(]/ { print $0 }
  ' "$REPO_ROOT/go.mod"
  echo
  echo "// Resolve the tool from the working tree under test, not the module"
  echo "// proxy: this harness gates the current checkout, not the last release."
  echo "replace $MODULE_PATH => $REPO_ROOT"
  echo
  # Forward every LOCAL (directory) replace directive the tool's own go.mod
  # declares — e.g. its sidecar nested module — the same way: a `replace
  # .../sidecar => ./sidecar` in a DEPENDENCY is ignored by the main module
  # (this is the exact reason the sidecar loader is its own module rather
  # than imported from elsewhere), so the stub needs its own copy, with the
  # relative path rewritten to resolve against $REPO_ROOT instead of the
  # stub's own directory.
  grep -E '^replace .+=> \./' "$REPO_ROOT/go.mod" | while IFS= read -r line; do
    nested_module=$(printf '%s' "$line" | awk '{print $2}')
    rel_path=$(printf '%s' "$line" | sed -E 's#.*=> \./#./#')
    echo "replace $nested_module => $REPO_ROOT/${rel_path#./}"
  done
} >"$STUB/go.mod"

# go.sum for the tool's own dependencies; the replaced module itself needs no
# entry (a directory replacement is not hashed).
cp "$REPO_ROOT/go.sum" "$STUB/go.sum"

# The hook wrapper, taken verbatim from README.md's "The hook script" section
# so this test exercises the documented script rather than a private variant.
extract_readme_hook() {
  awk '
    /^### The hook script/ { insection = 1; next }
    insection && /^```bash$/ { inblock = 1; next }
    inblock && /^```$/ { exit }
    inblock { print }
  ' "$REPO_ROOT/README.md"
}
extract_readme_hook >"$TREE/test/hooks/run-update-tester.sh"
grep -q 'crossplane-update-tester' "$TREE/test/hooks/run-update-tester.sh" ||
  abort "could not extract the hook script from README.md's '### The hook script' section"
chmod +x "$TREE/test/hooks/run-update-tester.sh"

ln -s run-update-tester.sh "$TREE/test/hooks/post-assert-network-v6.sh"
ln -s run-update-tester.sh "$TREE/test/hooks/post-assert-widget.sh"

# ─── the fake API server ────────────────────────────────────────────────────

go build -o "$BIN/faketestserver" "$REPO_ROOT/hack/faketestserver"

KUBECONFIG="$TMP/kubeconfig.yaml"
export KUBECONFIG
FAKESERVER_GEN=0

# start_fakeserver (re)starts the fake API server with a FRESH, freshly
# reseeded copy of every fixture object — killing any instance already
# running — and rewrites $KUBECONFIG to point at the new one. Called once at
# setup, and again before any scenario that must not observe another
# scenario's already-applied patch on the SAME fixture object (the "relative
# invocation" scenario re-runs network-v6.yaml's own hook a second time, and
# must see it in its pristine, unpatched state, exactly like a fresh
# `uptest` run would).
start_fakeserver() {
  if [ -n "$FAKESERVER_PID" ] && kill -0 "$FAKESERVER_PID" 2>/dev/null; then
    kill "$FAKESERVER_PID" 2>/dev/null || true
    wait "$FAKESERVER_PID" 2>/dev/null || true
  fi
  FAKESERVER_GEN=$((FAKESERVER_GEN + 1))
  local log="$TMP/faketestserver-$FAKESERVER_GEN.log"
  # The "backend" upper-cases this field when it stores it, exercising a
  # manifest entry whose `expect:` differs from its `value:`.
  "$BIN/faketestserver" -examples "$TREE/examples" -uppercase-fields routingHint \
    >"$log" 2>&1 &
  FAKESERVER_PID=$!

  # Race-free startup handshake: wait for the server's own "LISTEN
  # <host:port>" line rather than a fixed sleep, and fail loudly (rather than
  # hang) if it never appears — e.g. because the process exited immediately
  # on a bad flag.
  listen_addr=""
  for _ in $(seq 1 100); do
    if ! kill -0 "$FAKESERVER_PID" 2>/dev/null; then
      abort "faketestserver exited before printing a LISTEN line: $(cat "$log")"
    fi
    listen_addr="$(sed -n 's/^LISTEN //p' "$log" | head -1)"
    [ -n "$listen_addr" ] && break
    sleep 0.05
  done
  [ -n "$listen_addr" ] || abort "faketestserver did not print a LISTEN line within 5s"

  {
    echo "apiVersion: v1"
    echo "kind: Config"
    echo "clusters:"
    echo "- cluster: {server: http://$listen_addr}"
    echo "  name: fake"
    echo "contexts:"
    echo "- context: {cluster: fake, user: fake}"
    echo "  name: fake"
    echo "current-context: fake"
    echo "users:"
    echo "- name: fake"
    echo "  user: {}"
  } >"$KUBECONFIG"
}

start_fakeserver

# Small values keep the run quick: `converge` sleeps poll-interval * 1.5 per
# invocation, and `run` polls up to --timeout for a field that never lands.
export UPDATE_TESTER_POLL_INTERVAL="1s"
export UPDATE_TESTER_TIMEOUT="5"

# No cluster-backend override of any kind anywhere in this file: this harness
# exercises the DEFAULT in-process backend, driving the real binary against
# the fake API server started above — the first time this project's own test
# suite has ever done that.

echo "   tree ready (stub module, 3 examples, 2 post-assert symlinks, fake API server at $listen_addr)"

# ─── helpers ───────────────────────────────────────────────────────────────

# run_hook <symlink-name> <scenario> [cwd]
# Runs a post-assert symlink from a directory that is NOT the provider tree
# (unless cwd says otherwise). A symlink name containing a slash is used
# as-is, so a caller can reproduce uptest's relative invocation. Sets OUT and
# RC.
run_hook() {
  local link=$1 scenario=$2 cwd=${3:-$ELSEWHERE}
  local invocation="$TREE/test/hooks/$link"
  case "$link" in
    */*) invocation="$link" ;;
  esac
  OUT="$TMP/$scenario.out"
  set +e
  (cd "$cwd" && "$invocation") >"$OUT" 2>&1
  RC=$?
  set -e
}

# banners extracts the step banners the hook prints, one per line, as
# "<banner>\t<manifest path>".
banners() {
  sed -n 's/^==> update-tester: //p' "$1" |
    awk '{ path = $NF; $NF = ""; sub(/ +$/, "", $0); print $0 "\t" path }'
}

expect_banners() {
  local file=$1 label=$2 expected=$3 got
  got="$(banners "$file")"
  if [ "$got" = "$expected" ]; then
    ok "$label: step sequence is exactly as expected"
    return 0
  fi
  bad "$label: wrong step sequence"
  printf '   expected:\n%s\n   got:\n%s\n' \
    "$(printf '%s\n' "$expected" | sed 's/^/   | /')" \
    "$(printf '%s\n' "$got" | sed 's/^/   | /')" >&2
  return 1
}

# ─── 1. the stub module can host and execute the tool ──────────────────────

section "1. stub module with no Go files can host the tool"

VERSION_OUT="$TMP/version.out"
if (cd "$ELSEWHERE" && go -C "$STUB" tool crossplane-update-tester version) >"$VERSION_OUT" 2>&1; then
  if grep -q 'update-tester' "$VERSION_OUT"; then
    ok "go -C <stub> tool crossplane-update-tester version → $(tr -d '\n' <"$VERSION_OUT")"
  else
    bad "version subcommand produced unexpected output"
    dump "version output" "$VERSION_OUT"
  fi
else
  bad "'go -C <stub> tool crossplane-update-tester version' FAILED — the stub-module consumption pattern does not work"
  dump "version output" "$VERSION_OUT"
  printf '\nFAIL: nothing downstream can be meaningful; stopping.\n' >&2
  exit 1
fi

# ─── 2. the symlink runs the full 5-step sequence ──────────────────────────

section "2. post-assert-network-v6.sh symlink → 5-step sequence"

NETWORK_MANIFEST="$TREE/examples/network/network-v6.yaml"
run_hook post-assert-network-v6.sh network-v6

if [ "$RC" -eq 0 ]; then
  ok "hook exited 0"
else
  bad "hook exited $RC, expected 0"
  dump "hook output" "$OUT"
fi

expect_banners "$OUT" "network-v6" "$(
  printf 'converge\t%s\n' "$NETWORK_MANIFEST"
  printf 'check-external-name-prefix\t%s\n' "$NETWORK_MANIFEST"
  printf 'resolve-recover\t%s\n' "$NETWORK_MANIFEST"
  printf 'run\t%s\n' "$NETWORK_MANIFEST"
  printf 'post-update converge\t%s' "$NETWORK_MANIFEST"
)" || dump "hook output" "$OUT"

# The per-field results must actually be passes: a sequence that runs every
# step and reports FAIL inside them proves nothing.
if grep -q 'PASS: 2/2 tested' "$OUT"; then
  ok "run: both mutable fields PASSed (including the value≠expect normalisation)"
else
  bad "run: expected 'PASS: 2/2 tested' in the hook output"
  dump "hook output" "$OUT"
fi

# ─── 3. the annotation gate ────────────────────────────────────────────────

section "3. post-assert-widget.sh symlink → 3-step sequence (no prefix annotation)"

WIDGET_MANIFEST="$TREE/examples/widget/widget.yaml"
run_hook post-assert-widget.sh widget

if [ "$RC" -eq 0 ]; then
  ok "hook exited 0"
else
  bad "hook exited $RC, expected 0"
  dump "hook output" "$OUT"
fi

expect_banners "$OUT" "widget" "$(
  printf 'converge\t%s\n' "$WIDGET_MANIFEST"
  printf 'run\t%s\n' "$WIDGET_MANIFEST"
  printf 'post-update converge\t%s' "$WIDGET_MANIFEST"
)" || dump "hook output" "$OUT"

if grep -q 'check-external-name-prefix\|resolve-recover' "$OUT"; then
  bad "widget: an identity check ran for a manifest that does not opt in"
else
  ok "widget: neither identity check ran (annotation gate holds)"
fi

# ─── 4. the manifest path crossing `go -C` is absolute and per-symlink ──────

section "4. manifest path is absolute and symlink-specific (go -C edge)"

net_paths="$(banners "$TMP/network-v6.out" | cut -f2 | sort -u)"
widget_paths="$(banners "$TMP/widget.out" | cut -f2 | sort -u)"

if [ "$net_paths" = "$NETWORK_MANIFEST" ]; then
  ok "post-assert-network-v6.sh → $NETWORK_MANIFEST"
else
  bad "post-assert-network-v6.sh resolved to '$net_paths', expected '$NETWORK_MANIFEST'"
fi

if [ "$widget_paths" = "$WIDGET_MANIFEST" ]; then
  ok "post-assert-widget.sh → $WIDGET_MANIFEST"
else
  bad "post-assert-widget.sh resolved to '$widget_paths', expected '$WIDGET_MANIFEST'"
fi

# Two symlinks to ONE script resolved to two different manifests: `basename
# $0` yielded the symlink name, not the target. Had bash resolved it, both
# would have derived run-update-tester.yaml and neither would exist.
if [ -n "$net_paths" ] && [ -n "$widget_paths" ] &&
  [ "$net_paths" != "$widget_paths" ] &&
  ! printf '%s%s' "$net_paths" "$widget_paths" | grep -q 'run-update-tester'; then
  ok "basename \$0 yields the SYMLINK name, not the target (two symlinks → two manifests)"
else
  bad "the two symlinks did not resolve to distinct manifests — \$0 appears to have been resolved to its target"
fi

# ─── 5. the way uptest actually invokes it ─────────────────────────────────

section "5. relative invocation, exactly as uptest issues it"

# uptest runs the value of uptest.upbound.io/post-assert-hook, which is a path
# relative to the example's own directory. The hook must resolve the same
# absolute ROOT — and $0 must still be the symlink's name — when it is invoked
# that way rather than by absolute path.
UPTEST_INVOCATION="$(sed -n 's|.*uptest.upbound.io/post-assert-hook: ||p' "$NETWORK_MANIFEST")"
[ -n "$UPTEST_INVOCATION" ] || abort "could not read the post-assert-hook annotation from $NETWORK_MANIFEST"

# Fresh server: this re-runs network-v6.yaml's own full 5-step hook a second
# time, and section 2 above already patched that same object to its target
# field values — without a reset, the "run" step here would see fields
# already equal to their target and report NO-OP, not the pass this scenario
# is actually testing for.
start_fakeserver

run_hook "$UPTEST_INVOCATION" relative "$TREE/examples/network"

if [ "$RC" -eq 0 ]; then
  ok "'$UPTEST_INVOCATION' (run from examples/network) exited 0"
else
  bad "'$UPTEST_INVOCATION' (run from examples/network) exited $RC"
  dump "hook output" "$OUT"
fi

rel_paths="$(banners "$TMP/relative.out" | cut -f2 | sort -u)"
if [ "$rel_paths" = "$NETWORK_MANIFEST" ]; then
  ok "relative invocation resolved to the same absolute manifest: $NETWORK_MANIFEST"
else
  bad "relative invocation resolved to '$rel_paths', expected '$NETWORK_MANIFEST'"
  dump "hook output" "$OUT"
fi

# ─── 6. dual-scope event attribution, end to end ───────────────────────────
#
# dualscope-cluster.yaml and dualscope-namespaced.yaml share the SAME Kind
# and the SAME metadata.name — the unified example-manifest convention every
# dual-scope provider follows — differing only in scope and apiVersion
# group. This is the direct proof, through the real fake API server + runner
# (not just a unit-level fixture), that an update to one is never attributed
# to the other.

section "6. dual-scope event attribution (same Kind+Name, different scope) end-to-end"

CLUSTER_DUALSCOPE_MANIFEST="$TREE/examples/dualscope/dualscope-cluster.yaml"
NAMESPACED_DUALSCOPE_MANIFEST="$TREE/examples/dualscope/dualscope-namespaced.yaml"

DUALSCOPE_RUN_OUT="$TMP/dualscope-cluster-run.out"
set +e
(cd "$ELSEWHERE" && go -C "$STUB" tool crossplane-update-tester \
  run "$CLUSTER_DUALSCOPE_MANIFEST" --timeout 5) >"$DUALSCOPE_RUN_OUT" 2>&1
DUALSCOPE_RUN_RC=$?
set -e

if [ "$DUALSCOPE_RUN_RC" -eq 0 ]; then
  ok "cluster-scoped dualscope: run exited 0 (comment field updated, events recorded)"
else
  bad "cluster-scoped dualscope: run exited $DUALSCOPE_RUN_RC, expected 0"
  dump "run output" "$DUALSCOPE_RUN_OUT"
fi

if grep -q 'PASS: 1/1 tested' "$DUALSCOPE_RUN_OUT"; then
  ok "cluster-scoped dualscope: the mutable field PASSed (an UpdatedExternalResource event was recorded)"
else
  bad "cluster-scoped dualscope: expected 'PASS: 1/1 tested' in the run output"
  dump "run output" "$DUALSCOPE_RUN_OUT"
fi

DUALSCOPE_CONVERGE_OUT="$TMP/dualscope-namespaced-converge.out"
set +e
(cd "$ELSEWHERE" && go -C "$STUB" tool crossplane-update-tester \
  converge "$NAMESPACED_DUALSCOPE_MANIFEST" --poll-interval 1s --timeout 5s --readiness-timeout 5s) \
  >"$DUALSCOPE_CONVERGE_OUT" 2>&1
DUALSCOPE_CONVERGE_RC=$?
set -e

if [ "$DUALSCOPE_CONVERGE_RC" -eq 0 ]; then
  ok "namespaced dualscope sibling: converge PASSED despite the cluster-scoped sibling's recorded event"
else
  bad "namespaced dualscope sibling: converge FAILED — the cluster-scoped sibling's event bled across scope"
  dump "converge output" "$DUALSCOPE_CONVERGE_OUT"
fi

if grep -q '0 updates' "$DUALSCOPE_CONVERGE_OUT"; then
  ok "namespaced dualscope sibling: converge reported 0 updates (event isolation confirmed)"
else
  bad "namespaced dualscope sibling: expected a converge message reporting 0 updates"
  dump "converge output" "$DUALSCOPE_CONVERGE_OUT"
fi

# Reverse direction: now make the NAMESPACED sibling the one that genuinely
# updates, and confirm the CLUSTER-SCOPED sibling — which by now also has an
# events entry of its own, from the run above — still reports 0 updates.
# Isolation must hold in both directions, not just the one this scenario
# happened to exercise first.
DUALSCOPE_NS_RUN_OUT="$TMP/dualscope-namespaced-run.out"
set +e
(cd "$ELSEWHERE" && go -C "$STUB" tool crossplane-update-tester \
  run "$NAMESPACED_DUALSCOPE_MANIFEST" --timeout 5) >"$DUALSCOPE_NS_RUN_OUT" 2>&1
DUALSCOPE_NS_RUN_RC=$?
set -e

if [ "$DUALSCOPE_NS_RUN_RC" -eq 0 ] && grep -q 'PASS: 1/1 tested' "$DUALSCOPE_NS_RUN_OUT"; then
  ok "namespaced dualscope sibling: run also PASSed (it now has its own recorded event too)"
else
  bad "namespaced dualscope sibling: run did not PASS as expected"
  dump "run output" "$DUALSCOPE_NS_RUN_OUT"
fi

DUALSCOPE_CLUSTER_CONVERGE_OUT="$TMP/dualscope-cluster-converge.out"
set +e
(cd "$ELSEWHERE" && go -C "$STUB" tool crossplane-update-tester \
  converge "$CLUSTER_DUALSCOPE_MANIFEST" --poll-interval 1s --timeout 5s --readiness-timeout 5s) \
  >"$DUALSCOPE_CLUSTER_CONVERGE_OUT" 2>&1
DUALSCOPE_CLUSTER_CONVERGE_RC=$?
set -e

if [ "$DUALSCOPE_CLUSTER_CONVERGE_RC" -eq 0 ]; then
  ok "cluster-scoped dualscope sibling: converge PASSED despite the namespaced sibling's own recorded events"
else
  bad "cluster-scoped dualscope sibling: converge FAILED — the namespaced sibling's events bled across scope"
  dump "converge output" "$DUALSCOPE_CLUSTER_CONVERGE_OUT"
fi

if grep -q '0 updates' "$DUALSCOPE_CLUSTER_CONVERGE_OUT"; then
  ok "cluster-scoped dualscope sibling: converge reported 0 updates (isolation holds in both directions)"
else
  bad "cluster-scoped dualscope sibling: expected a converge message reporting 0 updates"
  dump "converge output" "$DUALSCOPE_CLUSTER_CONVERGE_OUT"
fi

# The re-homing of "every kubectl invocation was one the fake recognises"
# (formerly read off a per-scenario kubectl transcript): the fake API server
# itself logs any request path none of its handlers claim. Checked once,
# globally, at the point every scenario above has already run, across every
# generation the harness started (start_fakeserver is called once at setup
# and once more before section 5) — broader than the single-scenario check it
# replaces, since it now also covers every request issued by every section.
if grep -qE 'faketestserver: unhandled' "$TMP"/faketestserver-*.log; then
  bad "the fake API server rejected a request as unhandled"
  grep -h 'faketestserver: unhandled' "$TMP"/faketestserver-*.log | sed 's/^/   | /' >&2
else
  ok "every request the tool issued was one the fake API server recognises"
fi

# ─── summary ───────────────────────────────────────────────────────────────

printf '\n'
if [ "$FAILURES" -eq 0 ]; then
  printf 'PASS — %d checks, 0 failures\n' "$CHECKS"
  printf 'The stub-module + symlinked-hook consumption pattern works end to end.\n'
  exit 0
fi

printf 'FAIL — %d checks, %d failure(s)\n' "$CHECKS" "$FAILURES" >&2
exit 1
