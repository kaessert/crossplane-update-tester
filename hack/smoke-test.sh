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
#  5. A deliberately failing step exits non-zero AND aborts the steps after
#     it. A smoke test that only proved the happy path would happily pass a
#     hook that can no longer fail, which is the exact failure mode this tool
#     exists to catch.
#  6. Nothing in the whole kubectl transcript ever carried a relative manifest
#     path.
#
# It builds a throwaway fake provider tree under $TMPDIR, puts a stateful fake
# kubectl (hack/testdata/fake-kubectl.sh) first on PATH, and runs the real
# binary against it. The hook wrapper is extracted from README.md at run time
# rather than copied here, so this test validates the documented script and
# fails if the README drifts away from it.
#
# Requirements: go, bash, coreutils. No cluster, no network (the module cache
# is already warm if this repo builds).
#
# Usage: bash hack/smoke-test.sh    (from any working directory)
#        SMOKE_KEEP=1 bash hack/smoke-test.sh   keeps the tree and every
#        kubectl transcript for inspection instead of deleting them on exit.

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

cleanup() {
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
  echo "go 1.24.0"
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

# A third example, generated rather than checked in: it carries more mutable
# fields than the runner's event-burst ceiling (20), which is the only way to
# reach the controller-restart path — and therefore the only way to observe
# the `get pods` / `rollout restart` / `rollout status` argv.
mkdir -p "$TREE/examples/burst"
{
  echo "apiVersion: burst.example.crossplane.io/v1alpha1"
  echo "kind: Burst"
  echo "metadata:"
  echo "  name: example-burst"
  echo "  annotations:"
  echo "    crossplane.io/update-test: |"
  for i in $(seq -w 0 20); do
    echo "      - field: field$i"
    echo "        value: \"updated-$i\""
  done
  echo "spec:"
  echo "  forProvider:"
  for i in $(seq -w 0 20); do
    echo "    field$i: initial-$i"
  done
} >"$TREE/examples/burst/burst.yaml"

# The fake kubectl, first on PATH.
cp "$TESTDATA/fake-kubectl.sh" "$BIN/kubectl"
chmod +x "$BIN/kubectl"
export PATH="$BIN:$PATH"

# Small values keep the run quick: `converge` sleeps poll-interval * 1.5 per
# invocation, and `run` polls up to --timeout for a field that never lands.
export UPDATE_TESTER_POLL_INTERVAL="1s"
export UPDATE_TESTER_TIMEOUT="5"
# The "backend" upper-cases this field when it stores it, exercising a
# manifest entry whose `expect:` differs from its `value:`.
export SMOKE_UPPERCASE_FIELDS="routingHint"

echo "   tree ready (stub module, 3 examples, 2 post-assert symlinks, fake kubectl)"

# ─── helpers ───────────────────────────────────────────────────────────────

# run_hook <symlink-name> <scenario> [fail-mode] [cwd]
# Runs a post-assert symlink from a directory that is NOT the provider tree
# (unless cwd says otherwise), with its own state directory so each scenario's
# kubectl transcript stands alone. A symlink name containing a slash is used
# as-is, so a caller can reproduce uptest's relative invocation. Sets OUT, LOG
# and RC.
run_hook() {
  local link=$1 scenario=$2 mode=${3:-} cwd=${4:-$ELSEWHERE}
  local state="$TMP/state-$scenario"
  local invocation="$TREE/test/hooks/$link"
  case "$link" in
    */*) invocation="$link" ;;
  esac
  mkdir -p "$state"
  OUT="$TMP/$scenario.out"
  LOG="$state/kubectl.log"
  : >"$LOG"
  set +e
  (
    cd "$cwd" &&
      SMOKE_STATE="$state" SMOKE_FAIL_MODE="$mode" \
        "$invocation"
  ) >"$OUT" 2>&1
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

# assert_absolute_manifest_paths fails if any manifest path anywhere in a
# kubectl transcript is relative. The fake kubectl already refuses a relative
# `get -f` path, so this is the belt to that braces: it also covers paths in
# argv positions the fake does not inspect.
assert_absolute_manifest_paths() {
  local log=$1 label=$2 offenders
  offenders="$(tr ' ' '\n' <"$log" | grep -E '\.ya?ml$' | grep -v '^/' || true)"
  if [ -n "$offenders" ]; then
    bad "$label: kubectl was issued a RELATIVE manifest path: $(echo "$offenders" | tr '\n' ' ')"
    return 1
  fi
  ok "$label: every manifest path in the kubectl transcript is absolute"
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
  dump "kubectl transcript" "$LOG"
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
  dump "kubectl transcript" "$LOG"
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

# A namespaced resource must be addressed with -n on every resource call.
if grep -q -- '-n crossplane-e2e' "$TMP/state-widget/kubectl.log"; then
  ok "widget: namespaced resource addressed with -n crossplane-e2e"
else
  bad "widget: expected '-n crossplane-e2e' in the kubectl transcript"
  dump "kubectl transcript" "$TMP/state-widget/kubectl.log"
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

# ─── 4b. the way uptest actually invokes it ────────────────────────────────

section "4b. relative invocation, exactly as uptest issues it"

# uptest runs the value of uptest.upbound.io/post-assert-hook, which is a path
# relative to the example's own directory. The hook must resolve the same
# absolute ROOT — and $0 must still be the symlink's name — when it is invoked
# that way rather than by absolute path.
UPTEST_INVOCATION="$(sed -n 's|.*uptest.upbound.io/post-assert-hook: ||p' "$NETWORK_MANIFEST")"
[ -n "$UPTEST_INVOCATION" ] || abort "could not read the post-assert-hook annotation from $NETWORK_MANIFEST"

run_hook "$UPTEST_INVOCATION" relative "" "$TREE/examples/network"

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

assert_absolute_manifest_paths "$TMP/state-relative/kubectl.log" "relative invocation"

# ─── 5. the fake kubectl transcript ────────────────────────────────────────

section "5. kubectl transcript"

assert_absolute_manifest_paths "$TMP/state-network-v6/kubectl.log" "network-v6"
assert_absolute_manifest_paths "$TMP/state-widget/kubectl.log" "widget"

if grep -q 'FAKE-KUBECTL-ERROR' "$TMP"/state-*/kubectl.log; then
  bad "the fake kubectl rejected an invocation"
  grep -h 'FAKE-KUBECTL-ERROR' "$TMP"/state-*/kubectl.log | sed 's/^/   | /' >&2
else
  ok "every kubectl invocation was one the fake recognises"
fi

for expected in \
  "get -f $NETWORK_MANIFEST -o name" \
  "get events --all-namespaces -o json" \
  "wait networks.network.example.crossplane.io/example-network-v6 --for=condition=Ready --timeout=5s"; do
  if grep -qxF "$expected" "$TMP/state-network-v6/kubectl.log"; then
    ok "issued: $expected"
  else
    bad "expected kubectl invocation never issued: $expected"
  fi
done

# ─── 6. the event-burst reset path ─────────────────────────────────────────

section "6. event-burst reset (>20 mutable fields) restarts the controller"

BURST_OUT="$TMP/burst.out"
BURST_STATE="$TMP/state-burst"
mkdir -p "$BURST_STATE"
set +e
(cd "$ELSEWHERE" && SMOKE_STATE="$BURST_STATE" go -C "$STUB" tool crossplane-update-tester \
  run "$TREE/examples/burst/burst.yaml" --timeout 5) >"$BURST_OUT" 2>&1
BURST_RC=$?
set -e

if [ "$BURST_RC" -eq 0 ]; then
  ok "21-field run exited 0"
else
  bad "21-field run exited $BURST_RC, expected 0"
  dump "run output" "$BURST_OUT"
fi

for expected in \
  "get pods -n crossplane-system -l pkg.crossplane.io/revision -o jsonpath={range .items[*]}{.metadata.labels.pkg\\.crossplane\\.io/revision}{\"\\n\"}{end}" \
  "rollout restart deploy/provider-example-7f4c2b91d0ac -n crossplane-system" \
  "rollout status deploy/provider-example-7f4c2b91d0ac -n crossplane-system --timeout=3m0s"; do
  if grep -qxF "$expected" "$BURST_STATE/kubectl.log"; then
    ok "issued: $expected"
  else
    bad "expected kubectl invocation never issued: $expected"
    tail -20 "$BURST_STATE/kubectl.log" | sed 's/^/   | /' >&2
  fi
done

# ─── 6b. dual-scope event attribution, end to end ───────────────────────────
#
# dualscope-cluster.yaml and dualscope-namespaced.yaml share the SAME Kind
# and the SAME metadata.name — the unified example-manifest convention every
# dual-scope provider follows — differing only in scope and apiVersion
# group. This is the direct proof, through the real fake + runner (not just
# a unit-level fixture), that an update to one is never attributed to the
# other.

section "6b. dual-scope event attribution (same Kind+Name, different scope) end-to-end"

DUALSCOPE_STATE="$TMP/state-dualscope"
mkdir -p "$DUALSCOPE_STATE"
CLUSTER_DUALSCOPE_MANIFEST="$TREE/examples/dualscope/dualscope-cluster.yaml"
NAMESPACED_DUALSCOPE_MANIFEST="$TREE/examples/dualscope/dualscope-namespaced.yaml"

DUALSCOPE_RUN_OUT="$TMP/dualscope-cluster-run.out"
set +e
(cd "$ELSEWHERE" && SMOKE_STATE="$DUALSCOPE_STATE" go -C "$STUB" tool crossplane-update-tester \
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
(cd "$ELSEWHERE" && SMOKE_STATE="$DUALSCOPE_STATE" go -C "$STUB" tool crossplane-update-tester \
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
(cd "$ELSEWHERE" && SMOKE_STATE="$DUALSCOPE_STATE" go -C "$STUB" tool crossplane-update-tester \
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
(cd "$ELSEWHERE" && SMOKE_STATE="$DUALSCOPE_STATE" go -C "$STUB" tool crossplane-update-tester \
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

if grep -q 'FAKE-KUBECTL-ERROR' "$DUALSCOPE_STATE/kubectl.log"; then
  bad "dual-scope scenario: the fake kubectl rejected an invocation"
  grep -h 'FAKE-KUBECTL-ERROR' "$DUALSCOPE_STATE/kubectl.log" | sed 's/^/   | /' >&2
else
  ok "dual-scope scenario: every kubectl invocation was one the fake recognises"
fi

# ─── 7. a failing step must fail the hook and abort the rest ───────────────

section "7a. failure injection: reconcile loop (drifting atProvider)"

run_hook post-assert-network-v6.sh drift drift

if [ "$RC" -ne 0 ]; then
  ok "hook exited $RC (non-zero) when the resource never converged"
else
  bad "hook exited 0 despite a resource that never converges — a broken check would pass E2E silently"
  dump "hook output" "$OUT"
fi

if grep -q 'RECONCILIATION LOOP DETECTED' "$OUT"; then
  ok "reported: RECONCILIATION LOOP DETECTED"
else
  bad "expected 'RECONCILIATION LOOP DETECTED' in the output"
  dump "hook output" "$OUT"
fi

drift_banners="$(banners "$OUT" | cut -f1 | tr '\n' ',')"
if [ "$drift_banners" = "converge," ]; then
  ok "aborted at step 1: the remaining 4 steps never ran"
else
  bad "expected the hook to stop after 'converge', got: $drift_banners"
fi

if grep -qE '^patch ' "$TMP/state-drift/kubectl.log"; then
  bad "the hook patched the resource after the pre-check failed"
  dump "kubectl transcript" "$TMP/state-drift/kubectl.log"
else
  ok "no patch was issued after the failing pre-check"
fi

section "7b. failure injection: update never lands in status.atProvider"

run_hook post-assert-network-v6.sh stuck stuck

if [ "$RC" -ne 0 ]; then
  ok "hook exited $RC (non-zero) when a patched field never converged"
else
  bad "hook exited 0 despite a field whose value never reached status.atProvider"
  dump "hook output" "$OUT"
fi

stuck_banners="$(banners "$OUT" | cut -f1 | tr '\n' ',')"
if [ "$stuck_banners" = "converge,check-external-name-prefix,resolve-recover,run," ]; then
  ok "aborted inside 'run': the post-update converge never ran"
else
  bad "expected the hook to stop after 'run', got: $stuck_banners"
  dump "hook output" "$OUT"
fi

if grep -qE '^patch networks.* --type=merge -p \{"spec"' "$TMP/state-stuck/kubectl.log"; then
  ok "the failing step did reach the patch (it failed on the assertion, not before it)"
else
  bad "expected a spec patch in the transcript for the 'stuck' scenario"
  dump "kubectl transcript" "$TMP/state-stuck/kubectl.log"
fi

section "7c. failure injection: reconciliation loop via repeated update events (no atProvider drift)"

run_hook post-assert-network-v6.sh loop loop

if [ "$RC" -ne 0 ]; then
  ok "hook exited $RC (non-zero) when update events kept growing with no atProvider drift"
else
  bad "hook exited 0 despite update events growing on every poll — a broken check would pass E2E silently"
  dump "hook output" "$OUT"
fi

if grep -q 'RECONCILIATION LOOP DETECTED' "$OUT"; then
  ok "reported: RECONCILIATION LOOP DETECTED"
else
  bad "expected 'RECONCILIATION LOOP DETECTED' in the output"
  dump "hook output" "$OUT"
fi

# This is the direct proof that converge still COUNTS: the diagnostic must
# name a NON-ZERO new-event delta, not just an atProvider diff. A change that
# makes every delta 0 (this ticket's exact regression) would report no such
# diagnostic and this check would catch it.
if grep -qE '[1-9][0-9]* new update event\(s\) observed' "$OUT"; then
  ok "diagnostic names a non-zero new-event count — converge is still COUNTING events, not just diffing atProvider"
else
  bad "expected a 'N new update event(s) observed' diagnostic naming a non-zero delta"
  dump "hook output" "$OUT"
fi

loop_banners="$(banners "$OUT" | cut -f1 | tr '\n' ',')"
if [ "$loop_banners" = "converge," ]; then
  ok "aborted at step 1: the remaining 4 steps never ran"
else
  bad "expected the hook to stop after 'converge', got: $loop_banners"
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
