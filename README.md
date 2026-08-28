# crossplane-update-tester

A command-line tool that validates the `Update()` path of a Crossplane v2
provider during end-to-end test runs.

It drives everything through `kubectl` and annotations placed on the provider's
own example manifests. It contains no knowledge of any particular provider,
API group, or backend, so a single copy serves every provider that follows the
annotation convention.

Everything it asserts is derived from two sources:

- the example manifest under test (`apiVersion`, `kind`, `metadata.name`,
  `metadata.namespace`, and the annotations described below), and
- the live cluster, read via `kubectl` — the managed resource's
  `spec.forProvider`, `status.atProvider`, `metadata.generation`,
  `status.conditions[].observedGeneration`, and the Kubernetes Events that
  `crossplane-runtime`'s managed reconciler emits.

## Requirements

- Go 1.24 or newer. Consumers use the `tool` directive, which was added in
  Go 1.24; the module's own `go` directive is `1.24.0`.
- `kubectl` on `PATH` (or `KUBECTL` pointing at it), configured against the
  cluster under test.
- A cluster with the provider installed and its controller `Deployment` running
  in `crossplane-system`.
- Permission to list Events across all namespaces. Several checks read the
  aggregated Event count for the resource; without it those checks report a
  counting error rather than a verdict.

## Commands

```
update-tester run <manifest.yaml> [--timeout 120] [--poll-interval 60s]
update-tester converge <manifest.yaml> [--poll-interval 60s] [--ignore-fields a,b] [--timeout 120s] [--readiness-timeout 120s]
update-tester converge-all <m1.yaml,m2.yaml,...> [--poll-interval 60s] [--concurrency 8] [--timeout 120s] [--readiness-timeout 120s]
update-tester validate <manifest.yaml> [--types-file <types.go>] [--controller-dir <dir>] [--root <dir>]
update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
update-tester resolve-recover <manifest.yaml> [--timeout 120]
update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
update-tester roundtrip-verify <m1.yaml,m2.yaml,...> --backend <real|simulator> [--root <dir>] [--timeout 30]
update-tester hook <invocation-name> [--root <dir>] [--manifest <path>] [--skip-converge]
update-tester version
```

Flags may appear before or after the manifest path. Go's `flag` package stops
scanning at the first non-flag argument, which would otherwise cause
`run manifest.yaml --timeout 600` to silently drop the flag; the tool reorders
its arguments so that both orders behave identically.

Every check-running command exits non-zero when its check fails, so it can be
used directly as an E2E assertion.

### `run` — per-field update tests

For each entry in the manifest's `crossplane.io/update-test` annotation, `run`:

1. **Rejects no-ops.** It reads the field's current value (from
   `spec.forProvider`, falling back to `status.atProvider`) before patching. If
   the value already equals the test value, the patch would change nothing, the
   generation would not bump, and `Update()` would never be invoked — polling
   afterwards would re-observe the value that was already there and report a
   false pass. This is reported as `NO-OP`, a distinct failure, so the stale
   test value gets fixed.
2. **Records an Event baseline.** It sums the aggregated `count` of the
   resource's `UpdatedExternalResource` and `CannotUpdateExternalResource`
   Events. (`client-go` folds repeated identical Events into one object by
   incrementing `.count`, so counting Event objects is not the same as counting
   occurrences.)
3. **Patches the field** with a JSON merge patch under `spec.forProvider`,
   supporting dot-separated paths for nested fields.
4. **Drives two reconciles.** The first is the reconcile in which `Update()`
   runs, but its `Observe()` ran before `Update()`, so `status.atProvider` is
   still stale when it completes. A second reconcile is forced purely to obtain
   a fresh `Observe()`, so the result does not depend on the provider's
   background poll tick. The second is triggered by patching a private metadata
   annotation rather than by a status-only write: generated controllers
   typically watch with `resource.DesiredStateChanged()`, which reacts to
   annotation, label, or generation changes and filters status-only writes out
   entirely.
5. **Polls `status.atProvider`** for the expected value until `--timeout`.
6. **Requires evidence that `Update()` ran.** It re-counts the update Events. A
   field whose observed value matches the target but whose Event count never
   grew is reported as `NOT-EVIDENCED`, not `PASS`.
7. **Diffs for side effects.** The top-level keys of `status.atProvider` are
   compared before and after, excluding the field under test; anything else
   that moved is printed alongside the result.
8. **Asserts declared fields never move.** Every field named by the
   manifest's `assert-unchanged:` directive is checked against its pre-run
   baseline after every patch in the run. This is `run`'s own GATING check,
   not the diagnostic side-effect diff in point 7 above: a field named here
   fails the whole `run` invocation the moment it drifts, wherever in the
   run that happens. See "`crossplane.io/update-test`" below for the
   annotation syntax and what this exists to catch.

Point 6 is the reason this tool exists rather than a `kubectl patch` followed by
a value assertion. A value match alone cannot distinguish "the controller
updated the external resource" from "the value already matched, or something
else set it". The Event delta is a wall-clock-independent signal that the
reconciler's `Update()` path actually executed.

Two secondary behaviours follow from that design:

- **Event-burst resets.** `client-go`'s `EventBroadcaster` spam filter allows a
  burst of roughly 25 identical Events per object per controller process and
  then silently drops further ones. A resource with more mutable fields than
  that would produce false `NOT-EVIDENCED` results even though every `Update()`
  succeeded. Before the burst can be exhausted, `run` restarts the provider's
  controller `Deployment` — a new process starts with a fresh burst and emits a
  new Event object — and waits for the rollout to become ready.
- **Untrusted evidence.** If that restart fails, the burst state is unknown:
  a `PASS` could be masking a dropped Event and a `NOT-EVIDENCED` could be a
  false failure. Every field tested after a failed reset is reported as
  `UNTRUSTED` and counted as a failure, so a run can never print a clean
  "0 not-evidenced" summary on evidence it cannot vouch for.

A passing, evidenced field that took at least **half the provider's poll
interval** to converge is annotated `slow-observe`. It is still a pass — the
Event evidence is positive — but a result on that scale is worth a look at the
controller logs.

`run --poll-interval` (default `60s`) is what sets that bar, and it does
**nothing else**. `run` does not wait on the poll interval, does not poll on
it, and does not derive any timeout from it: how long a field test waits for a
value to appear is `--timeout`, and nothing about that changes when you pass
`--poll-interval`. The flag exists because "slow" is only meaningful relative
to the provider's own cadence — a provider polling every 10s and one polling
every 300s disagree by a factor of 30 about what a slow convergence is, and a
fixed 30-second bar would annotate nearly every pass of the first while never
annotating a genuinely poll-scale pass of the second. Passing a smaller value
makes the tool *report* more slow-observe annotations; it does not make the
tool *wait* less, and passing a larger one does not give a field more time to
converge.

The controller `Deployment` is found by reading the `pkg.crossplane.io/revision`
label off the controller Pod. If more than one provider package is installed,
the lookup is ambiguous and the run stops rather than restarting the wrong
controller; set `UPDATE_TESTER_PROVIDER_DEPLOYMENT` to disambiguate.

### `converge` — steady-state assertion

`converge` asserts that the resource settles instead of reconciling forever:

1. Poll until `metadata.generation` equals the `observedGeneration` carried by
   `status.conditions` (bounded by `--timeout`).
2. Poll until the `Synced` condition reports `True` at the resource's *current*
   `metadata.generation` (bounded by `--timeout`). Step 1 alone cannot see a
   reconcile that failed to persist a write: a late-initialization conflict
   still stamps `observedGeneration` equal to the current generation on the
   `Synced` condition it marks `False`, so "settled" above can already be true
   on a pass that never succeeded. Unlike step 3, a timeout here **fails the
   check**, reporting `RESOURCE NOT IN STEADY STATE`. A resource whose
   reconciler emits no `Synced` condition at all is treated as not applicable
   and does not block.
3. Poll until the `Ready` condition reports `True` (bounded by
   `--readiness-timeout`, defaulting to the same 120s as `--timeout`). A
   resource that is still coming up mirrors live readiness facts into
   `status.atProvider`, and snapshotting before it settles would read those
   as drift. On timeout this step proceeds anyway rather than failing
   outright — it only narrows the window in which that can happen — and adds
   a diagnostic line noting the timeout without dropping anything else the
   check finds.
4. Poll until the provider controller Pod is at least 15s old (bounded by a
   120s timeout, reported as `RESOURCE NOT IN STEADY STATE` on expiry). A
   just-(re)started controller Pod may still be running its own cold-start
   reconcile burst against every resource it watches, and a baseline taken
   mid-burst would read that burst as drift the instant it lands.
5. Snapshot `status.atProvider`, the generation, and the update-Event count.
6. Wait `--poll-interval * 1.5`, which guarantees at least one full reconcile
   cycle.
7. Assert that `status.atProvider` is unchanged (minus `--ignore-fields`),
   that the generation is unchanged, that **zero** new update Events were
   recorded, and that `Ready` is still `True`. A `Ready` flap at this point is
   reported as its own diagnostic, separate from the `atProvider` diff.

**Controller-Pod-restart awareness.** If the provider controller Pod is
replaced during the wait — an event-burst reset (see `run` above), an OOM
kill, an operator action, a package re-install — the Pod that comes back
re-observes every resource with cold caches and writes to it again, which
looks identical to a real reconciliation loop from the outside even though it
is not. `converge` detects this by comparing the controller Pod's identity
before and after the wait; on a mismatch it discards the spoiled window,
re-arms against the new Pod, and re-measures rather than blaming the
controller for its own restart. This can happen at most twice before the
check gives up and reports `CONVERGENCE INCONCLUSIVE`, naming how many
restarts it observed — a resource whose window is spoiled on every attempt is
never silently reported as a pass.

A resource stuck in a perpetual update loop reports `Ready` on every cycle, so
an assertion on `Ready` — the assertion an E2E harness makes by default — passes
happily while the provider writes to the backend forever. The non-zero update
Event count over a fixed window is what makes the loop visible.

Use `--ignore-fields` for genuinely dynamic observed fields (server timestamps,
rolling counters). If a resource cannot converge for a structural reason, put a
`converge-skip:` line in its annotation with the reason; the check then reports
`CONVERGE-SKIP` and exits zero.

### `converge-all` — shared-window fleet convergence

`converge-all` runs the same steady-state assertion as `converge`, but for many
manifests against a single shared observation window instead of one window per
resource.

Running the per-resource form against N manifests spends
`N * (poll-interval * 1.5)` asleep, back to back, and every one of those N
windows observes a DIFFERENT stretch of wall-clock time — drift that only
appears while several controllers are reconciling at once falls between the
windows and is never seen. `converge-all` observes every resource over the
same stretch instead, which is both faster and a strictly stronger check.

This is safe because convergence performs `kubectl` reads only — no patches,
no controller restarts. No target can perturb another's observation, so every
target's arm and assert step can be interleaved freely.

The steps:

1. **Arm** every target concurrently (bounded by `--concurrency`). Arming
   snapshots a baseline the same way `converge` steps 1-5 do above. A target
   that resolves early — `CONVERGE-SKIP`, an unsettled generation, an error —
   is recorded immediately and takes no further part in the shared window.
2. **One shared wait.** Arming is not instantaneous: each target waits for its
   own generation to settle and for `Ready`, so baselines land at different
   times. The window closes at `max(ArmedAt) + max(convergeWait)` across every
   armed target. Anchoring on the LATEST baseline and the LONGEST per-target
   wait is what guarantees every target is observed for at least its own
   required duration, even when targets declare different `--poll-interval`
   values — every target but the last-armed one is therefore observed for
   strictly *more* than its own minimum, never less.
3. **Assert** every armed target concurrently, each against its own elapsed
   time since it was armed — not the shared wait duration. A target armed
   earlier was under observation for longer, and counting update Events over a
   shorter window than was actually observed would under-report a loop on
   exactly those resources. A target whose own controller Pod restarted
   during the shared wait re-arms and re-measures independently of every
   other target, per the controller-Pod-restart awareness described above —
   it does not reopen or extend the shared window for anyone else.

`--concurrency` (default `8`) bounds how many targets are armed or asserted at
once. Each step shells out to `kubectl`, so this caps concurrent processes, not
anything the API server itself experiences — an unbounded fan-out over a large
catalog would spawn one process per resource per phase.

`--ignore-fields` here is a FLEET-WIDE default, unioned onto every target's own
per-manifest `ignore-fields:` directive rather than replacing it. It is
lossless only when every resource in the run genuinely shares the same
exclusion set — using it alone across a fleet with divergent per-resource
exclusions (a load-balancer's `forwardRules` field means nothing to a network
resource) would silently widen every resource's exclusion set to the union of
all of them. Prefer the per-manifest directive for anything not shared by the
whole fleet; see ticket dcbdabdb for the mechanism that added it.

Manifest paths are accepted either as one comma-separated list or as repeated
positional arguments — both forms are equivalent:

```
update-tester converge-all a.yaml,b.yaml,c.yaml
update-tester converge-all a.yaml b.yaml c.yaml
```

`converge-all` is a separate entry point, not part of the `hook` sequence —
`hook` (below) runs its own two per-resource `converge` steps by default, and
drops both only when passed `--skip-converge`. It exists for a provider that
wants one shared window across many resources instead of `hook`'s per-resource
ones.

### `validate` — offline coverage check

`validate` needs no cluster. It scans a generated `types.go` for the
`<Kind>Parameters` struct (skipping every other struct declaration in the
file, not only other `*Parameters` structs) and reports, per field, whether
the manifest's annotation covers it.

`--types-file` is optional. When omitted, `validate` resolves the types file
itself: it derives the resource's scope from the manifest's apiVersion group
(a group ending in `.m.crossplane.io` is namespaced, everything else is
cluster-scoped — never the manifest's filename) and searches
`--root/apis/<scope>/` for the one file that declares
`type <Kind>Parameters struct`. Zero matches or more than one match is a
hard failure naming the Kind, the resolved scope, and the directory
searched — never a silent fallback to a wrong-but-existing path.
`--root` defaults to the working directory, matching `roundtrip-diff`,
`roundtrip-verify` and `hook`. Passing `--types-file` explicitly always
overrides discovery.

- `tested` / `skipped` — the field appears in the annotation with a value, or
  with a structured `skip:` reason (see "`skip:` reasons" below).
- `skipped-unstructured` — the field's `skip:` is the pre-migration free-prose
  string form rather than a structured reason. Still counts as coverage,
  exactly like `skipped`, but is reported under its own status so a fleet
  migrating its old free-prose entries to the structured form has a
  burn-down count distinct from genuinely resolved coverage.
- `immutable` — the field carries a `self == oldSelf` validation marker, so it
  has no update semantics.
- `reference-plumbing` — the field is a generated `*Ref` / `*Refs` /
  `*Selector` companion whose base value field is present in the same
  struct. A scalar base field gets the singular `*Ref`; a list-typed
  (`[]string`) base field gets the plural `*Refs`; either cardinality also
  gets one `*Selector`.
- `tested-via-switch` — the field has no entry of its own, but is named in
  another non-skipped entry's `clear:` list. That entry's merge patch proved it
  clearable, never proved it settable, so it is credited under its own status
  and never folded into `tested`. A `skip:` entry's `clear:` list grants no
  credit, and a field with its own direct entry keeps that entry's status.
- `clear-target-unknown` — a `clear:` list names something that is not a
  declared field on this type (a typo, or a field renamed or removed since the
  entry was written). The command exits non-zero.
- `withValues-target-unknown` — a `withValues:` map names something that is not
  a declared field on this type — the same typo/rename failure mode
  `clear-target-unknown` guards against, applied to the other directive that
  writes a sibling in the same merge patch. The command exits non-zero. Unlike
  `clear:`, a `withValues:` sibling never earns coverage credit (see
  `tested-via-switch` above) even when it does name a real field: its
  post-patch value is never asserted by the runner, only written.
- `guarded-assert-unchanged` — the field has no entry of its own, but is named
  (as its own top-level field, or as the first segment of a nested
  `status.atProvider` path) by the annotation's top-level `assert-unchanged:`
  directive. A field the directive names DIRECTLY cannot also carry its own
  `field:`/`skip:` entry — the two are rejected as an overlap at parse time —
  so a manifest choosing this stronger, actively-enforced guard has nothing
  left to write for the ordinary coverage credit. A field reached only as the
  first segment of a nested path is not that overlap and may still carry its
  own entry; where it does, that entry's status wins and this one is never
  substituted for it. Counts as coverage, but
  reported under its own status: assert-unchanged proves the field never
  drifts, never that a specific new value can be written to it, so it is
  never folded into `tested` or `skipped`.
- `MISSING` — none of the above; the command exits non-zero.

This is what stops an annotation from quietly falling behind the API type as
fields are added.

`validate` also checks a second, unrelated property, and fails the command
for it independently of coverage: an annotation can report zero `MISSING`
fields and `validate` can still exit non-zero. Coverage asks "does an
annotation exist for this field?"; this second check asks "can the field's
new value actually be read back afterwards?" — and answers no for a specific,
detectable shape.

An `expect:` or `value:` object (or list of objects) that names a top-level
key absent from the field's generated `*Observation` struct is printed as

```
✗ customDataTypes: UNOBSERVABLE — key(s) customDataTypesRef ...
```

and the command exits non-zero. This fires when the entry's expectation is
built around a `*Ref` / `*Selector` cross-resource reference — those fields
are input-only by construction and the generator never mirrors them into
`atProvider`, so no amount of polling will ever observe the value the test
asserted. The test cannot pass by waiting longer; it cannot pass at all as
written. When you see this, do not add a `skip:` entry — that only hides the
gap. Instead, replace the reference key in the entry with an `expect:` that
names the value actually resolved into `atProvider` (the field the reference
points at, not the reference itself), so the check reads back a value the API
genuinely returns.

This check is deliberately conservative, so its silence is not a guarantee.
Resolving the target field's Observation-side shape tries, in order. Both
steps search every non-`_test.go` `.go` file in the same directory — the
same Go package — rather than a filename pattern: Go guarantees at most one
declaration of a given name per package, so any production source file is an
authoritative place to look, and `_test.go` files are excluded because an
external `_test` package may legally declare a same-named struct that is not
part of the production API.

1. `<ElemType>Observation` by name — first in `--types-file`, then in every
   other non-`_test.go` `.go` file in the same directory. This is the fast path
   for a generator layout that emits a distinct Observation companion struct,
   and it is not limited to the file named on the command line: a
   `tcp-loadbalancer`'s `originPoolsWeights` field carries the same
   unobservable `poolRef` key that an `http-loadbalancer`'s
   `defaultRoutePools` field does, and both are flagged even though
   `ViewsOriginPoolWithWeightObservation` is declared only in
   `zz_http_loadbalancers_types.go` while the tcp-loadbalancer manifest's
   `--types-file` points at `zz_tcp_loadbalancers_types.go` — the sibling
   search in step 1 finds it there.
2. Failing that, the field of the same JSON name declared on
   `<Kind>Observation` itself — resolved the same way, in `--types-file`
   then its non-`_test.go` `.go` siblings — followed by resolving THAT field's
   own declared type, again in `--types-file` then its siblings. This
   handles a generator layout that reuses the identical struct on both the
   Parameters and the Observation side of a field, with no separate
   `Observation` companion at all: `provider-f5xc`'s `ServicePolicyRule`
   declares `PolicyMatcherTypeBasic` for both
   `ServicePolicyRuleParameters.DomainMatcher` and
   `ServicePolicyRuleObservation.DomainMatcher` verbatim, and there is no
   `PolicyMatcherTypeBasicObservation` anywhere in the package. A
   `domainMatcher` update-test entry that omits `exactValues` — a
   non-omitempty member of that same reused struct — is flagged.

What genuinely remains unresolved, so the "silence is not a guarantee"
caveat still has a true basis: a `skip:` entry, a dotted field path (e.g.
`cookieParams.authHmac.primKeySecretRef`), a scalar expectation, a
cross-package type this checker cannot inspect the shape of, and a field
that neither step above can resolve at all — absent from both
`<ElemType>Observation` (searched in `--types-file` and its siblings) and
`<Kind>Observation` (searched the same way). A clean `UNOBSERVABLE`-free run
is not proof every entry is observable — only that neither resolution step
could prove otherwise.

`validate` also flags an `expect:`/`value:` object that leaves a sibling key
of the create-time `spec.forProvider` object unaddressed by the patch. RFC
7386 (`kubectl patch --type=merge`, what the `run` command actually applies)
preserves any top-level key of an existing object the patch never mentions,
so a partial-object `value:` update silently carries that sibling forward —
and if the effective expectation does not separately name it, the runner's
whole-value comparison against `status.atProvider` can never be satisfied by
construction. This is printed as

```
✗ headerMatcher: SIBLING-SURVIVES — key(s) invertMatch are absent from value: and will survive the RFC 7386 merge patch unaddressed; either add expect: recording the merged shape, or set the surviving key(s) to null if they are a mutually exclusive alternative being replaced
```

and the command exits non-zero. This check runs entirely offline and consults
no Go types — the manifest's own `spec.forProvider` and
`crossplane.io/update-test` annotation are enough — though `validate` itself
still resolves a types file (explicit `--types-file`, or discovered under
`--root`) for its mutable-field coverage report.

`validate` also flags an `expect:`/`value:` object that omits a top-level key
the target field's generated Observation struct declares WITHOUT
`omitempty`. Such a member always marshals into `status.atProvider` — as an
explicit `null` when the API never set it — so an expectation that never
names it can never satisfy the runner's whole-value comparison, even for a
field being set for the very first time (absent from `spec.forProvider`
entirely at create time, the one case `SIBLING-SURVIVES` above has no
create-time object to reason about). This is printed as

```
✗ domainMatcher: INCOMPLETE-EXPECT — key(s) exactValues are non-omitempty members of the generated Observation struct absent from expect:/value:, and will marshal (as null, if unset) regardless; add expect: naming the resolved value
```

and the command exits non-zero. The remedy is the same as `UNOBSERVABLE`'s:
add an `expect:` naming the value the backend actually returns, not a
`skip:` — that only hides the gap. A key `SIBLING-SURVIVES` already flagged
for the same entry is never reported a second time here; it is the same
underlying defect with the same remedy, not a second, independent one.

`validate` runs a further check, opt-in via `--controller-dir <dir>`, naming
the resource's controller package directory. Without it, this check is
skipped entirely — everything above still runs.

A controller can register a [go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp)
`Transformer` that clears a field before comparing desired against observed,
because the backend echoes a default value back on every element the caller
never set one for (e.g. an explicit `invert_match: false`, or an empty-string
`description`) and a verbatim comparison against that echo would report a
permanent diff — the provider calling `Update()` every reconcile on an
otherwise idle resource. Because the desired side never encodes that default
explicitly, the cleared field's generated struct member necessarily carries
`omitempty`, which is exactly the shape `INCOMPLETE-EXPECT` above excludes.
An `expect:`/`value:` object omitting such a member — at any depth in its
nested object tree, not only the top level — is printed as

```
✗ blockedClients: SERVER-ECHOED — key(s) invertMatch are cleared by a registered normalizer ...
```

and the command exits non-zero. The remedy is the same as `UNOBSERVABLE`'s:
add an `expect:` naming the value the backend actually returns.

This check is a BACKSTOP, not the primary method, for a question no amount
of types-file analysis can answer: which `omitempty` Observation members a
live backend echoes back anyway is a property of the backend, not of the
generated Go, and a silent `SERVER-ECHOED` run is not proof an expectation is
complete — only that this static check found nothing. Where the backend
supports a cheap create/delete, one live probe answers the question exactly;
this check exists for the providers where it does not (an SDK or CLI backend
with no inexpensive way to stand up and tear down a probe resource).
Measured on a live tenant, `provider-f5xc`'s `service_policy_rules`
`path` field: `INCOMPLETE-EXPECT` names four non-omitempty keys
(`prefix_values`, `regex_values`, `suffix_values`, `transformers`), but the
backend actually echoes six — `invert_matcher` and `encoded_path_matcher`
carry `omitempty` in the generated Go and so are silently absent from that
static prediction, yet the server sets both regardless of what the caller
sent. This check derives which struct/field pairs are server-echoed straight
from the controller source — every `cmp.Transformer("...", someFunc)`
registration it finds, resolving `someFunc`'s parameter type and the field it
nils out — rather than from a hand-maintained list, so it cannot go stale
independently of the normalizer it describes. It matches a struct in the
manifest's nested object tree against that source struct by field-name-set
fingerprint (the generated struct must declare at least every field the
hand-written one does), since the two are named independently by the
generator and the controller author. A controller package with no such
registration leaves this check inert — no findings, no error.

`validate` also flags a non-`skip:` entry whose `value:` is byte-for-byte the
same shape already present at the entry's field path in the manifest's own
create-time `spec.forProvider`. A merge patch that repeats a value the API
server already holds makes no change for it to persist — `metadata.generation`
never bumps, so the controller's `Update()` path is never invoked. This is the
same pre-patch guard the `run` command itself applies immediately before
building the patch, checked here offline instead of against a live read, so
the defect is caught before a cluster and an admission slot are spent
rediscovering something the manifest already said. This is printed as

```
✗ enableApiDiscovery: GUARANTEED-NO-OP — value: already equals the create-time spec.forProvider value at this field, so the merge patch changes nothing, metadata.generation never bumps, and Update() is never invoked; change value: to something the resource does not already hold
```

and the command exits non-zero. The remedy is to change `value:` to something
the resource does not already hold at creation — the same fix a `NO-OP`
result from a live `run` would prompt, found here without spending the run.

This check runs entirely offline and consults no Go types — the manifest's
own `spec.forProvider` and `crossplane.io/update-test` annotation are enough
— exactly like `SIBLING-SURVIVES`; `validate` itself still resolves a types
file (explicit `--types-file`, or discovered under `--root`) for its
mutable-field coverage report. It compares
`value:` itself, never an `expect:` override: that is exactly what the live
pre-patch guard compares against the value already on the resource, so
substituting `expect:` here would make this check disagree with the runtime
verdict it exists to predict.

It is deliberately conservative, the same bar as this tool's other offline
checks. An entry is flagged only when every EARLIER non-skipped entry in the
same manifest left the field's top-level name untouched — neither as its own
`field:`, nor named in its own `clear:` list. Entries run in order against
one live resource, so an earlier entry naming the same top-level field may
have changed what is actually there by the time this entry's patch is built;
this check has no way to know whether that earlier entry converged, so it
declines to guess rather than risk a false positive.

### `expect-skeleton` — expect: key-set generator (dev aid)

`expect-skeleton` needs no cluster and no manifest — only `--kind` and
`--field`, plus the same `types.go` `validate` reads. It is the write side of
`INCOMPLETE-EXPECT`: rather than reporting that an existing `expect:` block is
missing keys, it prints the full key set a brand-new one would need, before
you write it.

```
$ update-tester expect-skeleton zz_service_policy_rules_types.go --kind ServicePolicyRule --field domainMatcher
expect: # skeleton for domainMatcher — keys only, fill in the real value(s)
  exactValues: TODO
  regexValues: TODO
```

It reuses exactly the same resolution `validate`'s `INCOMPLETE-EXPECT` check
does — the fast `<ElemType>Observation` path, then the `<Kind>Observation`
reuse-layout fallback — so its output is only as complete as that check's own
"silence is not a guarantee" caveat above already states. Every key printed
is set to the literal placeholder `TODO`, never a guessed value: this command
cannot know what the backend actually returns, only which keys the generated
Observation struct requires an `expect:` block to name.

No output keys printed (a comment line instead) means one of two things it
cannot tell apart: the field genuinely needs no `expect:` block at all (every
Observation member carries `omitempty`), or its Observation-side shape could
not be resolved. Run `validate` against a real manifest naming the field to
tell those two apart.

### `check-external-name-prefix` — identity guard (opt-in)

Requires `crossplane.io/expect-external-name-prefix` on the manifest; without
it the command errors rather than silently passing. It asserts that the live
resource's `crossplane.io/external-name` annotation starts with the declared
prefix.

This is for resources whose backend models more than one object type behind a
single Kubernetes kind. An identity search issued against the wrong object type
finds nothing, the reconciler creates a second object, and the resource still
reports `Ready` — invisible to a plain `Ready` assertion, but visible in the
external-name prefix.

### `resolve-recover` — ref-less identity recovery (opt-in)

Gated on the same annotation. A standing create/update/delete lifecycle always
carries a non-empty `crossplane.io/external-name`, so it never exercises the
code path that resolves identity by searching the backend.
`resolve-recover` forces it: it pauses reconciliation with
`crossplane.io/paused`, strips the external-name annotation, unpauses, and waits
for the recovery reconcile.

It passes only if **both** signals hold:

1. the recovered external-name still carries the declared prefix, and
2. exactly one `CreatedExternalResource` Event exists for the resource across
   its whole lifecycle.

Signal 1 alone is not enough: a wrong-type search that finds nothing and falls
through to a duplicate `Create()` still derives the object type from the desired
spec, so the duplicate's external-name is correctly prefixed too. The second
`CreatedExternalResource` Event is what distinguishes "found the existing
object" from "created a second one".

### `roundtrip-diff` — spec/status round-trip report (advisory)

Whether a field round-trips faithfully through the backend is not knowable
from the generated types alone: the API may default a field that was never
sent, canonicalise a value, or silently drop something the client submitted.
`roundtrip-diff` answers that by observation: for each manifest, it walks the
matching CRD's OpenAPI schema (found under `--root/package/crds`) to
enumerate every declared `spec.forProvider` field, then classifies each one
against the live object's `spec.forProvider` value and its
`status.atProvider` mirror:

- `equal` — present on both sides, same value.
- `value-changed` — present on both sides, different value.
- `present-in-spec-absent-from-mirror` — the client set it; the mirror never
  reports it.
- `defaulted-by-server` — the client never set it; the backend supplied a
  value anyway.
- `present-in-mirror-absent-from-spec` — the field exists only in the
  `atProvider` schema (no `forProvider` counterpart at all), and the mirror
  reports a value for it.

A field never populated on either side is not reported — there is nothing to
observe yet. A schema field declared `type: array` is a leaf, whole-value
comparison: its element schema is never descended into, so a nested
difference inside one array element (a backend adding a sibling key to one
list entry) surfaces as `value-changed` on the array's own path, not as a
separate per-element sub-path.

This command is entirely read-only (`kubectl get`, nothing else) and, like
the check it replaced, is always advisory: a finding is printed, never a
reason to fail. `converge-all` inlines the same report next to a **FAILING**
target's own verdict line when `UPDATE_TESTER_ROOT` is set — see
"Environment" below — so the finding is visible at the moment the run
actually failed instead of requiring a second, separate invocation.

### `roundtrip-verify` — must-test denominator (enforcing)

`roundtrip-diff` is advisory by design and never fails on what it finds.
`roundtrip-verify` is its enforcing counterpart: it computes the SAME
five-way classification for every manifest given to it, on every
invocation — never only when something else already failed — and then
checks every field the manifest's `crossplane.io/update-test` annotation
`skip:`s against its own live row. It fails when a "must-test" field's
waiver does not hold up.

`--backend <real|simulator>` is required on every invocation, declaring
whether the provider's E2E path runs against a live backend or a simulator
(e.g. vcsim) with no real-backend arm at all — there is no default and no
inference from a provider name, endpoint, or URL. See the `backend` /
`seed` bullet below for how the declaration surfaces in the report.

The must-test set is derived from the classification, not asserted by hand:

- `equal` — the value round-trips faithfully. The only classification a
  `skip:` waiver is cheap against, whatever reason it names.
- `value-changed` / `defaulted-by-server` — the backend changed or defaulted
  the value. These can never be waived, under any reason — including
  `write-only`: a field that changed or was defaulted round-trips through
  the mirror by definition, which already disproves a write-only claim.
- `present-in-spec-absent-from-mirror` — the client set it, the mirror never
  reports it. Must be tested, or waived `skip: {reason: write-only}` — this
  is the one row shape that reason's claim is checked against. **Exception:**
  a field the served CRD declares CEL-immutable (an
  `x-kubernetes-validations` rule matching `self == oldSelf`, on the field's
  own schema node or inherited from an ancestor object node whose whole
  subtree is marked immutable) is removed from the must-test set entirely —
  a write-only-shaped field the backend can never echo back, by
  construction, forever, so demanding a live update assertion for it would
  be a false positive. It is reported under its own `immutable (excluded)`
  status, in both the JSON `excluded` array and the human-readable render,
  never silently dropped and never counted as a `skip:` waiver. A `skip:`
  entry against such a field — of ANY reason — is accepted without needing
  to be `write-only`, because the field was never in the denominator to
  begin with.
- `present-in-mirror-absent-from-spec` — not a spec field at all, so it is
  never part of the denominator.
- **No row at all** (the field was never populated on either side of the
  live object) — a legacy free-prose `skip:` is a finding here: with no row
  to confirm anything, a free-prose sentence is exactly the unchecked claim
  this check exists to stop crediting cheaply. `write-only` is ALSO a
  finding here, even though it is structured: its citation IS a
  `present-in-spec-absent-from-mirror` row, and a no-row field gives it
  nothing to resolve against. The other four structured reasons
  (`union-arm`, `covered-elsewhere`, `vendor-defect`, `fixture-missing`) are
  accepted — each carries its own citation (`sibling:`, `by:`,
  `evidence:` + `ticket:` for either of the last two) resolved elsewhere
  (see `validate`'s `skip: reasons` checks below).

CEL-immutability never excuses `value-changed` or `defaulted-by-server`: a
backend that changes or defaults a field the CRD declares immutable is a
contradiction the tool exists to catch, not a reason to excuse it, so those
two classifications stay in the must-test set and stay eligible to become a
finding — immutable or not.

A field with no `skip:` entry at all is untouched by this command: direct
testing is already proven by `run`, and a field neither tested nor skipped
is already reported `MISSING` by `validate`.

Each manifest's report is printed as one JSON object per line — `kind`,
`name`, every row (`path`, `classification`, `specFound`/`specValue`,
`mirrorFound`/`mirrorValue`, and `immutable` when the field is
CEL-immutable), the must-test set size, every excluded (CEL-immutable)
field, and every unresolved finding — followed by a human-readable
rendering of the same must-test size, excluded fields, and findings. The
report is emitted for every manifest this command can reach a live object
for, whether or not any finding turns up: the JSON line is never
conditioned on failure, unlike `converge-all`'s advisory inline report.

The report additionally carries a cell-denominator breakdown, entirely
additive to everything above and NEVER part of the command's exit-code
decision:

- `cells` — every `equal` cell (one `(classification, container shape,
  direction)` grouping) this run observed, its full member list, which
  members this run chose as representatives (a size-scaled rotation:
  small cells reach full coverage in a couple of runs, large ones within
  about ten), which members were credited by cell membership instead of
  tested, and which members are permanently promoted (`sticky`) because a
  past run's representative failed. `value-changed`, `defaulted-by-server`
  and `present-in-spec-absent-from-mirror` never collapse into a cell this
  way — each of those stays individually tested and individually
  reported, exactly as the must-test findings above already require.
- `containerClear` — for every declared container-typed (list or free-form
  map) `spec.forProvider` leaf, one of THREE states, never two:
  - **ineligible** — the leaf's removal direction can never be exercised at
    all, so it is excluded from the denominator entirely rather than
    counted as a gap to close. Reported with its own `reason`, re-derived
    from the CRD's schema on every run (never a hardcoded list, per-leaf
    annotation, or per-provider config), so a schema change that removes
    the shape or the rule puts the leaf back into the denominator
    automatically. Three reasons, and only three:
    - a CEL-immutable field — the leaf's own schema node, or an ancestor
      object node enclosing it, carries an `x-kubernetes-validations`
      rule requiring `self == oldSelf` (including the "immutable once
      set" spelling, `size(oldSelf) == 0 || self == oldSelf`). That is a
      strictly stronger block than the CEL-required reason below: it
      rejects EVERY mutation of the field rather than one specific patch
      shape, so nulling, emptying and setting any other value are all
      rejected identically and no clear-direction test can ever reach the
      backend. Derived from the CRD schema alone — the same
      ancestor-inheriting walk the `immutable` exclusion in the must-test
      denominator already uses — and never from the field's name or any
      `skip:` waiver;
    - a crossplane reference-resolution field — a cross-resource
      `*Selector`'s free-form `matchLabels` map, or a `*Refs` list of
      reference items — resolved entirely by the platform before a
      request ever reaches this provider, and never mirrored back in
      `status.atProvider`. Derived from the field's SCHEMA SHAPE, never
      from its name, and cross-checked against the live mirror schema so
      a field that genuinely IS echoed back is never excluded on shape
      alone;
    - a field an `x-kubernetes-validations` rule requires whenever the
      object's `managementPolicies` is at its own schema default. For a
      free-form MAP leaf this alone is enough: nulling it is rejected by
      admission, and `value: {}` is an RFC-7386 no-op that leaves every
      existing member untouched, so no clear-direction test can ever reach
      the backend. For a LIST leaf, `value: []` is itself an admissible
      wholesale-replacement clear under RFC-7386 (the same route "covered"
      below credits), so a CEL-required list stays ELIGIBLE unless its own
      schema ALSO declares `minItems > 0`, or a second CEL rule requires
      `<path>.size() > 0` — either one closes the `value: []` route the same
      way the CEL rule's `has()` guard closes nulling, and only then is the
      leaf ineligible. The reported `reason` always names the actual
      blocker (`minItems: N`, the `size()` rule's own text, or the map's
      RFC-7386 no-op) rather than a generic "admission rejects nulling it".
  - **covered** — ANY test entry actually exercises its removal direction:
    a `clear:` list naming it exactly, a `clear:` list naming an ANCESTOR
    of its dotted path (an RFC-7386 merge-patch null removes the whole
    subtree beneath that ancestor, this leaf included), a directly-tested
    map value that nulls one of its own member keys, or a self-tombstone:
    the leaf's own entry setting `value: null` explicitly, or (for a LIST
    leaf only) an empty list value (`value: []`) — see "Whole-field
    tombstones without a sibling field" below for when the self-tombstone
    route is needed, and for why a map leaf's own `value: {}` does NOT
    qualify.
  - **uncovered** — neither of the above; a gap this manifest could close.

  A leaf is never reported as both ineligible and covered for either of
  the CEL-derived reasons — CEL-immutable, or CEL-required (map, or a
  list whose empty-clear route is also
  closed) — that combination means an existing manifest entry disagrees
  with the ineligibility predicate, and the report surfaces that
  disagreement explicitly (`containerClearError`) rather than silently
  preferring one side. The reference-resolution reason is exempt from that
  error: a reference-resolution field carries no CEL rule guarding it, so
  an ancestor `clear:` tombstone that incidentally sweeps up a
  reference-resolution descendant alongside a genuinely tested sibling is
  never rejected by admission and is not evidence the predicate is wrong —
  it is simply reported `ineligible`, `covered: false`, with no error.

  This whole breakdown is advisory ONLY: it is informational in every
  report and never turns the command's exit code non-zero, regardless of
  how much or how little of a manifest's container-typed surface is
  covered, ineligible, or in conflict.
- `waivers` — every `skip:`-carrying entry, bucketed against its own live
  row as `false` (the row is a must-test classification the tool already
  disproves; the waiver is hiding a real deviation) or `keep` (nothing to
  test against, a confirmed legitimate exception, or the row is `equal` —
  a value set at CREATE round-tripped, which says nothing about whether
  the UPDATE path the waiver guards can be exercised at all, so `equal` is
  never grounds to delete a waiver). `keep`'s cost and priority are the
  waiver's evidence tier, not this bucket.
- `backend` / `seed` — `backend` restates the run's declared `--backend`
  (`real` or `simulator`; the flag is required, with no default and no
  inference from a provider name or endpoint). Every `cells` entry restates
  whether it was satisfied under a `simulator` declaration
  (`simulatorSatisfied`), so a reader never has to cross-reference the
  top-level field. `seed` is the pseudo-random rotation schedule's own
  seed for this run, so a past run's exact representative choice can be
  reproduced.

Like `roundtrip-diff`, this command is entirely read-only.

### `hook` — the post-assert entry point

`hook` derives the manifest path from the name it was invoked under and then
runs the full post-assert sequence for it: `converge`, then — only when the
manifest carries `crossplane.io/expect-external-name-prefix` —
`check-external-name-prefix` and `resolve-recover`, then `run`, then `converge`
again. The final convergence check matters: the field updates just proved values
round-trip, which is not the same as proving the controller stopped reconciling.

Derivation rules, relative to `--root`:

| Invocation name | Manifest |
|---|---|
| `post-assert-<resource>.sh` | `examples/<resource>/<resource>.yaml` |
| `post-assert-<resource>-namespaced.sh` | `examples/<resource>/<resource>-namespaced.yaml` |
| `post-assert-<resource>-ns.sh` | `examples/<resource>/<resource>-namespaced.yaml` |

Two rules are resolved against the filesystem rather than by string
manipulation:

- `-ns` is shorthand for `-namespaced`, but it is also a legitimate ending for
  a resource name (a DNS NS record resource is naturally `record-ns`). The
  literal reading wins whenever `examples/<slug>/` exists as a directory.
- A resource directory may hold several example pairs for the same kind — an
  alternate variant such as `examples/network/network-v6.yaml` beside
  `examples/network/network.yaml`. When the primary derivation names no
  existing file, the last hyphenated segment of the slug is stripped and the
  same leaf filename is looked for in that shorter directory. The fallback runs
  only when the primary path is missing, so it can never reinterpret a symlink
  that already resolves.

Setting `MANIFEST` overrides derivation entirely, which is useful when debugging
a manifest that does not follow the convention.

#### `--skip-converge`

Drops both `converge` steps from the sequence above, leaving
`[check-external-name-prefix, resolve-recover] -> run`. Nothing else changes.

This is **opt-in, per provider, and defaults to false**. It exists for a
provider that asserts convergence some other way — for example, one shared
observation window run once for many resources instead of one window per
resource here — and must never be turned on for a provider that has no such
replacement: doing so silently deletes that provider's convergence coverage.
The flag is retired (removed from the CLI) once every provider that consumes
this tool runs such a replacement and no longer needs the per-resource
`converge` steps at all.

## Manifest annotations

### `crossplane.io/update-test`

A YAML block scalar holding a list of per-field entries, optionally preceded by
three top-level directive lines: `converge-skip:`, `assert-unchanged:` and
`ignore-fields:`.

| Key | Meaning |
|---|---|
| `field` | Required. Field name under `spec.forProvider`; dot-separated for nested fields. |
| `value` | The value to patch in. Required unless `skip` is set — but "required" means the `value:` key must be PRESENT, not non-null: an explicit `value: null` (or a bare `value:` with nothing after the colon) is accepted as a deliberate whole-field tombstone in its own right, distinct from the key being absent entirely — see "Whole-field tombstones without a sibling field" below. |
| `expect` | Optional. The value expected in `status.atProvider` when the backend normalises what it stores. Defaults to `value`. |
| `skip` | Optional. Either a free-prose string (the legacy form, reported as `SKIPPED` and credited under `validate`'s `skipped-unstructured` status) or a structured mapping with a `reason:` from a closed set (reported as `SKIPPED` and credited under `validate`'s `skipped` status — see "`skip:` reasons" below). Either way the entry still counts as coverage for `validate`. |
| `clear` | Optional. A list of OTHER top-level `spec.forProvider` field names nulled in the SAME merge patch that sets `field`'s value, so a union modeled as separate top-level fields switches arms atomically. Only valid when `field` itself is top-level (not dotted); a dotted entry, or one naming `field` itself, is rejected at parse time — even for a `field` with no other sibling to name; see "Whole-field tombstones without a sibling field" for that case's own route. |
| `withValues` | Optional. A mapping of OTHER top-level `spec.forProvider` field names to an explicit, non-null literal value set in the SAME merge patch that sets `field`'s value — see "Backend-coupled fields: converging a source field and its derived field in one patch" below. Only valid when `field` itself is top-level (not dotted); a dotted key, a key naming `field` itself, or a key also present in this entry's own `clear` list is rejected at parse time. |
| `ignoreMapKeys` | Optional. A list of top-level member keys excluded, on BOTH sides, from `run`'s equality check between `expect` (or `value`, when `expect` is unset) and the live `status.atProvider` value — see "`ignoreMapKeys:` — excluding a provider-injected map member" below. Mutually exclusive with `skip` (there is no comparison left for it to affect). |
| `ignoreListElementKeys` | Optional. A list of per-element member keys excluded, on BOTH sides and from EVERY element, from `run`'s equality check between `expect` (or `value`, when `expect` is unset) and the live `status.atProvider` value, for a list-of-objects field — see "`ignoreListElementKeys:` — excluding a provider-injected per-element member" below. Mutually exclusive with `skip` (there is no comparison left for it to affect). |

None of `converge-skip: <reason>`, `assert-unchanged: <fields>` or
`ignore-fields: <fields>` is valid YAML as a sibling of top-level sequence
items, so all three lines are extracted before the rest of the block is
parsed as a sequence. Each must be unindented.

#### `skip:` reasons

A structured `skip:` is a mapping with a `reason:` from a closed set. An
unrecognised reason is a parse-time error naming the valid set; `reason:
immutable` is rejected by name, since immutability is already derived
mechanically from the field's own `self == oldSelf` validation marker rather
than declared here.

| reason | required keys | resolved offline by `validate`? |
|---|---|---|
| `union-arm` | `sibling:` naming another field | yes — `sibling:` must be a field declared on the same `<Kind>Parameters` struct |
| `covered-elsewhere` | `by:` as `<path>#<field>` | yes — that manifest and field must exist, and the named entry must itself be directly tested (not skipped, not missing, and not a `covered-elsewhere` cycle) |
| `vendor-defect` | `evidence:` and `ticket:` | no — both keys are checked for presence only |
| `fixture-missing` | `evidence:` and `ticket:` | no — both keys are checked for presence only |
| `write-only` | none | no |

```yaml
crossplane.io/update-test: |
  - field: allowList
    skip:
      reason: union-arm
      sibling: ruleList
  - field: firewallGroupId
    skip:
      reason: fixture-missing
      evidence: "no fixture backend exposes a second firewall group to move this field between"
      ticket: VU-FW-FIXTURE
  - field: dnsVolterraManaged
    skip:
      reason: vendor-defect
      evidence: "HTTP 400 'Change of domain type ... is not supported'"
      ticket: FX-DNS-DELEGATION
```

The pre-existing free-prose string form (`skip: "some reason"`) still parses
and still counts as coverage, but is reported under `validate`'s distinct
`skipped-unstructured` status rather than `skipped` — see the status list
above.

#### `skip:` dispositions

A structured `skip:` may also carry an optional `disposition:` — a second,
independent axis alongside `reason:`. Where `reason:` says WHY no test
exists, `disposition:` says HOW that reason could be — or already has
been — checked, so a reader (or a future tool) can tell a claim that is
mechanically re-checkable from one that is only a human's word. It is
**authored, or it is absent** — nothing infers a disposition from `reason:`
or `evidence:` prose, and an unrecognised value is a parse-time error naming
the valid set.

| disposition | means | required keys |
|---|---|---|
| `statically-provable` | decidable from the repo alone — a CRD schema validation rule, the resource's own controller source, or the example manifest's own data — with no cluster and no live call | none |
| `one-live-patch` | a claim about backend runtime behaviour, resolvable by firing ONE request with no lasting consequence (a rejection leaves state unchanged; an acceptance is undoable by a further, similarly-priced request) — including a claim whose evidence was already gathered and is recorded in the reason's own prose | none |
| `declared-exclusion` | firing the `one-live-patch`-shaped probe is ITSELF the irreversible or destructive act, or damages state shared with other runs, so no mechanical check can ever confirm it — a standing human commitment instead | `declared-by:` and `reconfirm:`, both required |
| `defect` | the repo's own artifacts contradict the stated reason, or the reason names nothing checkable at all | none |

```yaml
crossplane.io/update-test: |
  - field: osId
    skip:
      reason: vendor-defect
      evidence: "os_id is structurally absent from the update request the controller sends"
      ticket: VU-OSID-STATIC
      disposition: statically-provable
  - field: displayName
    skip:
      reason: vendor-defect
      evidence: "HTTP 409, and the display name is permanently reserved regardless of response code"
      ticket: IAM-DISPLAYNAME
      disposition: declared-exclusion
      declared-by: a human
      reconfirm: "2027-01-01"
```

`disposition:` is currently report-only: `roundtrip-verify`'s
`containerClear` findings surface, per uncovered container-typed leaf,
whether its own `skip:` entry (if any) carries a disposition and which, but
nothing folds it into an exit code today, and an absent `disposition:` is
reported as absent rather than defaulted to any of the four values.

#### Whole-field tombstones without a sibling field

`clear:` folds a null for OTHER top-level fields into the merge patch that
sets `field`'s own value, so it can never null `field` itself — that
restriction stays in force even for a `Kind` whose `spec.forProvider`
declares no other top-level field at all to name. A container-typed field
(a list or a free-form map) that IS the only top-level field on its `Kind`
still needs a way to prove its removal direction is tested, so two
additive routes exist, authored directly on the field's own entry instead
of a sibling's:

```yaml
crossplane.io/update-test: |
  - field: rules
    value: null
```

An explicit `value: null` (equivalently `value: ~`, or a bare `value:`
with nothing after the colon — all three are the same YAML null) builds
exactly the same `{"spec":{"forProvider":{"rules":null}}}` merge-patch
body a `clear:` entry would produce for a sibling, but targets `field`
itself. It is accepted only because the entry deliberately wrote the
`value:` key — an entry that omits `value:` altogether still fails with
"value is required unless skip is set", exactly as before this existed.
This recipe needs no `expect:`: once the patch has cleared the field,
`status.atProvider` no longer has anything at that path, which reads back
exactly as the field being present-but-null does, and both are accepted as
satisfying a `null` expectation.

The second route reaches the same coverage credit with an ordinary value,
no null involved, but applies ONLY to a list-typed field:

```yaml
crossplane.io/update-test: |
  - field: tags
    value: []
```

An empty list replaces the field wholesale under RFC-7386 merge-patch
semantics — a non-object value is never merged member-by-member, it is
substituted outright — so it removes every existing member exactly as a
null tombstone does, while still reading as a normal, non-null test value.

**This does NOT extend to a free-form map field.** `value: {}` on a map
field is not a tombstone: RFC 7386 recurses into an object-valued patch
member and merges it key-by-key against the live object, so an empty
object names no member to remove and the live map survives completely
unchanged. A map-typed field with no sibling to host a `clear:` entry has
exactly one route to proven removal coverage — the explicit `value: null`
recipe above.

Either working route only ever credits the exact field carrying it;
neither has an ancestor-walk analogue the way a `clear:` tombstone on an
ancestor object does; see the `containerClear` cell-denominator breakdown
above for how this is reported.

#### Backend-coupled fields: converging a source field and its derived field in one patch

`clear:` folds a `null` for a sibling into the same merge patch; `withValues:`
folds an explicit, non-null LITERAL for a sibling into that same patch
instead. It exists for a pair of fields that are coupled on the BACKEND, not
merely in the CRD schema: a deprecated source field and the field the backend
derives from it on every write. A backend of this shape re-derives the
derived field from whatever the source field currently holds server-side on
ANY patch that does not also carry an explicit value for the source field —
independent of what that same patch's own value for the derived field is. So
genuinely converging the derived field to a new value, once the source field
has ever been set, requires the SAME patch to also carry a real, non-null
value for the source field. `clear:` cannot express this: a `null` on an
optional field is dropped from the outgoing request by `omitempty` rather
than read as "clear this", which is exactly the "no change" outcome that
fails to converge the source field at all.

```yaml
crossplane.io/update-test: |
  - field: tags
    value: []
    withValues:
      tag: ""
```

This converges `tags` to an empty list and `tag` to an empty string in ONE
merge patch — `{"spec":{"forProvider":{"tag":"","tags":[]}}}` — rather than
two sequential patches, each of which would independently succeed at the
Kubernetes level while leaving a window where the two fields disagree on the
backend.

A field named in `withValues:` must not also appear in this entry's own
`clear:` list — the two directives would disagree about what that one
sibling ends up holding in the same patch, so the combination is rejected at
parse time rather than left to resolve however a map happens to iterate.

#### `assert-unchanged:` — silent-wipe guard

A comma-separated list of dot-separated `status.atProvider` field paths that
must hold the SAME value for the entire `run`, regardless of which other
field is being patched:

```yaml
crossplane.io/update-test: |
  assert-unchanged: legacyRuleList
  - field: comment
    value: "updated"
```

This exists for a backend that silently defaults an omitted field on every
write — for example, a PUT that omits a union field causes the backend to
reset it to an empty default and still return `200`, even though the request
never mentioned that field at all. A value-only assertion on the field being
patched cannot see this, because the field the backend corrupts is never the
one under test; `run`'s own side-effect diff (see its point 7 above) surfaces
the same drift, but only as a printed diagnostic, not a failure. Declaring the
vulnerable field here makes `run` check it after every patch in the run and
GATE — fail the whole invocation — the moment it moves, attributing the
failure to whichever field test's patch was in flight when the drift first
appeared. A field that never drifts is still printed, so a reviewer scanning a
green log can see the guard ran.

A field may not appear in both `assert-unchanged:` and as an entry's own
`field:` — patching a field and asserting it never changes are contradictory
requests, so that combination is rejected at parse time, before any cluster is
touched.

A declared path that does not resolve on the object — a typo, a stray
container segment, a field the backend has not populated yet — is also
rejected, at the start of `run`, before any field test patches anything. An
unresolvable path is never treated as an implicit empty baseline: doing so
would make the field compare `""` against `""` for the whole run regardless
of what the backend actually does, which is a guard that always reports
green without ever having measured anything.

The mechanism is generic: it reads whatever field paths the manifest declares
and compares live values against a baseline, with no knowledge of any
particular resource, API, or backend.

#### `ignore-fields:` — per-resource convergence exclusions

A comma-separated list of TOP-LEVEL `status.atProvider` field names excluded
from a convergence check's snapshot diff, for THIS resource only. A
dot-separated path is rejected at parse time rather than silently excluding
nothing — the diff matches only a top-level key, so a nested path like
`ruleChoice.legacyRuleList` would never match anything it excludes:

```yaml
crossplane.io/update-test: |
  ignore-fields: latestBackup
  - field: comment
    value: "updated"
```

This is the per-resource counterpart to `converge-all`'s fleet-wide
`--ignore-fields` flag. `converge-all` covers several manifests — potentially
several different resource *kinds* — in one invocation, and the flag applies
its set to every one of them, which is lossless only when every resource in
that invocation shares one exclusion set (true for a provider where the
excluded field is uniformly a status field, false the moment two resources
need different exclusions — a database's `latestBackup` timestamp has nothing
to do with an instance's `kvm`/`powerStatus`/`serverStatus`). Declaring the
exclusion here keeps it attached to the resource it describes:
`converge-all` unions the flag's set onto each manifest's own directive
rather than replacing it, so a field excluded here for one resource is never
silently excluded for another sharing the same invocation. The single-resource
`converge` command reads this directive too, unioning it with its own
`--ignore-fields` flag the same way — this is the only path any provider's
E2E hook actually runs (`hook` -> `converge`, never `converge-all`), so a
directive that reached only `converge-all` would never take effect for a
provider's real test run.

#### `ignoreMapKeys:` — excluding a provider-injected map member

A per-entry list of top-level member keys excluded, on BOTH sides, from
`run`'s equality check between the entry's effective expectation (`expect`,
or `value` when `expect` is unset) and the live `status.atProvider` value.

It exists for a map-typed field whose live value carries a member the
PROVIDER itself writes — an identity stamp, a server-managed marker —
alongside the keys the manifest actually manages. Without it, `expect` for
such a field has to predict that provider-written value verbatim, and a
value derived from something that does not exist until the resource is
created (a `metadata.uid`-derived stamp, for example) can never appear in a
static example manifest — the whole-map comparison is unsatisfiable by
construction, and there is no dotted-field workaround: a null-tombstone
removal of one map key can only ever be expressed through a top-level,
non-dotted entry (nesting the null inside a non-nil map value), and a
top-level entry's `run` comparison is always against the ENTIRE map.

`ignoreMapKeys` closes that gap without touching how the patch itself is
built: `value` and `expect` still describe add, update and null-tombstone
removal exactly as they always have; only the comparison ignores the named
keys, on both sides, so `expect` never has to mention — let alone guess —
the provider-injected member's value.

```yaml
crossplane.io/update-test: |
  - field: extAttrs
    value:
      Team: "platform"        # add
      Environment: "staging"  # update an existing key
      Deprecated: null        # null-tombstone removal
    expect:
      Team: "platform"
      Environment: "staging"
      # Deprecated is correctly absent (removed), and the provider's own
      # identity-stamp member is correctly absent too, without ever having
      # to name its live value.
    ignoreMapKeys: [OwnerStamp]
```

#### `ignoreListElementKeys:` — excluding a provider-injected per-element member

A per-entry list of member keys excluded, on BOTH sides and from EVERY
element, from `run`'s equality check between the entry's effective
expectation (`expect`, or `value` when `expect` is unset) and the live
`status.atProvider` value.

It is the list-shaped counterpart of `ignoreMapKeys`: it exists for a
list-of-objects field whose live elements each carry a member the PROVIDER
itself assigns per element — a server-generated id on each rule of a
firewall-rule list, for example — alongside the keys the manifest actually
manages. `ignoreMapKeys` cannot reach this: it strips a key from the top
level of a map-shaped comparison, but a list's own elements sit one level
beneath that, so a provider-assigned per-element key would still force
`expect` to predict a value that does not exist until the element is
created on the backend, and can therefore never appear in a static example
manifest.

`ignoreListElementKeys` closes that gap the same way `ignoreMapKeys` does:
`value` and `expect` still describe the list exactly as they always have —
an RFC 7386 merge patch replaces a list-typed field wholesale, so there is
no partial-element update to express — only the comparison ignores the
named keys, on every element, on both sides, so `expect` never has to
mention — let alone guess — any element's provider-assigned member.

```yaml
crossplane.io/update-test: |
  - field: firewallRules
    value:
      - port: 443
        protocol: tcp
      - port: 8080
        protocol: tcp
    expect:
      - port: 443
        protocol: tcp
      - port: 8080
        protocol: tcp
      # Each element's server-assigned id is correctly absent, without
      # ever having to name its live value.
    ignoreListElementKeys: [id]
```

#### Worked example

```yaml
apiVersion: network.example.crossplane.io/v1alpha1
kind: Network
metadata:
  name: example-network-v6
  annotations:
    uptest.upbound.io/post-assert-hook: ../../test/hooks/post-assert-network-v6.sh
    # Opt in to the two identity checks. Only meaningful for resources whose
    # backend models several object types behind one kind.
    crossplane.io/expect-external-name-prefix: "ipv6network/"
    crossplane.io/update-test: |
      - field: comment
        value: "Updated by update-tester (IPv6)"
      - field: extAttrs
        skip: "optional map field, low update-test value"
      - field: parentCidr
        skip: "create-time-only allocation input, mutually exclusive with the static CIDR this example uses"
spec:
  forProvider:
    networkView: default
    network: 2001:db8:1::/64
    comment: Managed by Crossplane (IPv6)
  providerConfigRef:
    name: default
```

Because per-field tests apply as cumulative patches, a single field the backend
rejects can leave the resource in a state where later fields in the same run
also fail. Fields that a minimal example cannot legally set — allocation inputs,
feature flags whose companion list is empty — belong in `skip` with the reason
stated.

The annotation is looked up across every document of a multi-document manifest,
so an example that ships a companion `Secret` or `ProviderConfig` alongside the
managed resource is handled correctly; the annotated document is the one tested.

### `crossplane.io/expect-external-name-prefix`

Optional and opt-in. Its presence enables `check-external-name-prefix` and
`resolve-recover`; its value is the required prefix of the resource's
`crossplane.io/external-name` annotation. Manifests that omit it skip both
checks.

## Consuming it from a provider repository

Do **not** vendor the source. Add a stub module with no Go files of its own:

```
# <provider>/tools/update-tester/go.mod
module github.com/<org>/<provider>/tools/update-tester

go 1.24.0

tool github.com/kaessert/crossplane-update-tester
```

Create it with:

```console
$ mkdir -p tools/update-tester && cd tools/update-tester
$ go mod init github.com/<org>/<provider>/tools/update-tester
$ go get -tool github.com/kaessert/crossplane-update-tester@v0.1.0
```

`go get -tool` adds the `tool` directive and pins the exact version in the stub
module's own `go.mod` and `go.sum`. Commit both.

Then invoke it from the provider root:

```console
$ go -C tools/update-tester tool crossplane-update-tester run /abs/path/to/examples/network/network.yaml
```

### Why a stub module instead of a `tool` directive in the root `go.mod`

A `tool` directive adds the tool's requirements to the module graph of whatever
module declares it. Putting it in the provider's root `go.mod` would make this
tool's dependencies part of the provider's dependency graph — visible to
`go mod tidy`, to `go list -m all`, to vulnerability scanners reporting on the
shipped provider, and to anything resolving a shared dependency's version.
An isolated module under `tools/update-tester/` keeps that graph the provider's
own. The tool is still built from source, still version-pinned, and still
reproducible — just accounted for separately.

### Sharp edge: `go -C` changes the working directory

`go -C tools/update-tester` runs the Go command, and therefore the tool process,
with `tools/update-tester` as the working directory. **Any manifest path passed
across that boundary must be absolute.** A relative path that is correct from
the provider root resolves against `tools/update-tester/` inside the tool and
fails — or, worse, finds a different file. The same applies to `--types-file`
and `--root`.

### The hook script

`test/hooks/run-update-tester.sh` becomes a wrapper that `exec`s the `hook`
subcommand:

```bash
#!/usr/bin/env bash
# Every test/hooks/post-assert-<resource>.sh is a symlink to this script; the
# invocation name ($0) selects the manifest. ROOT is absolute because `go -C`
# runs the tool with tools/update-tester as its working directory.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec go -C "$ROOT/tools/update-tester" tool crossplane-update-tester \
  hook "$(basename "$0")" --root "$ROOT"
```

`$0` is the symlink name, not its target — bash does not resolve symlinks
before `basename` — which is what lets one script serve every resource:

```console
$ ln -s run-update-tester.sh test/hooks/post-assert-network.sh
$ ln -s run-update-tester.sh test/hooks/post-assert-network-v6.sh
$ ln -s run-update-tester.sh test/hooks/post-assert-network-namespaced.sh
```

Each example manifest then points its
`uptest.upbound.io/post-assert-hook` annotation at its own symlink.

## Environment

| Variable | Used by | Effect |
|---|---|---|
| `KUBECTL` | all cluster commands | Path to the `kubectl` binary. Defaults to `kubectl` from `PATH`. |
| `MANIFEST` | `hook` | Absolute path to a manifest, overriding derivation from the invocation name. Intended for debugging. |
| `UPDATE_TESTER_TIMEOUT` | `hook` | Default timeout for the checks the hook runs, where no flag is given. |
| `UPDATE_TESTER_POLL_INTERVAL` | `hook` | Provider poll interval. Passed to both convergence checks, which wait `interval * 1.5`, and to `run`, which uses it only to calibrate the `slow-observe` annotation (half the interval). One variable, so both checks are measured against the same cadence. |
| `UPDATE_TESTER_IGNORE_FIELDS` | `hook` | Comma-separated `atProvider` field names excluded from the snapshot diff. Forwarded only to the two `converge` steps — `run`, `check-external-name-prefix` and `resolve-recover` do not define `--ignore-fields` and would reject it. For a resource with a field the backend populates asynchronously and independently of any controller write (e.g. a one-time timestamp set once the first automated action completes), that field's presence would otherwise fail every convergence check as if it were reconciliation drift. |
| `UPDATE_TESTER_PROVIDER_DEPLOYMENT` | `run` | Name of the provider controller `Deployment` to restart for event-burst resets. Required when more than one provider package is installed, since the Pod-label lookup is then ambiguous. |
| `UPDATE_TESTER_EVENT_BURST_CEILING` | `run` | Overrides how many update-triggering field tests are attempted before proactively restarting the provider controller to earn back a fresh event-spam-filter burst (default 20, calibrated to client-go's stock burst of ~25). Set this higher on a controlplane that raises the provider's own event burst, so the run stops paying a restart it no longer needs. Any unset, unparseable, zero or negative value falls back to the default — a typo here degrades to the default rather than aborting the run. |
| `UPDATE_TESTER_ROOT` | `converge-all` | Provider repository root — the directory holding `package/crds/`. Unset (the default), `converge-all` behaves exactly as it always has. Set, a FAILING target's `roundtrip-diff` report is inlined next to its own verdict line. Deliberately an environment variable rather than a `--root` flag, so `converge-all --help` never changes based on whether a caller sets it. |

## Versioning and compatibility

Releases are tagged semver. Consumers pin an exact version in their stub
module's `go.mod` and `go.sum`, so an E2E run resolves the same source on every
machine and a new release cannot change a provider's test behaviour until that
provider updates its pin:

```console
$ cd tools/update-tester
$ go get -tool github.com/kaessert/crossplane-update-tester@v0.2.0
```

The compatibility surface is the CLI: subcommand names, flags, the two manifest
annotations, and exit codes. Breaking changes to any of them take a major
version. `update-tester version` prints the version in use, which is worth
including in E2E logs when a result needs explaining after the fact.

## Contributing

- `go test ./...` must pass. The packages are unit-testable without a cluster:
  `kubectl` invocations go through an injectable exec function, and the
  filesystem-dependent derivation rules are tested against real temporary
  trees.
- **Keep it dependency-light.** The only non-stdlib dependency is
  `gopkg.in/yaml.v3`, and it should stay that way. This tool is built from
  source inside every E2E run of every consuming provider; each added
  dependency is downloaded, compiled, and audited by all of them. A new
  dependency needs a reason that outweighs that.
- No provider-specific, backend-specific, or API-specific logic. Everything the
  tool needs about a resource comes from the manifest annotations and the
  cluster. If a check cannot be expressed that way, it belongs in the provider,
  not here.
- Run `gofmt`, `go vet ./...` and `golangci-lint run ./...` before opening a
  pull request; CI runs all three, plus `go test ./... -count=1 -race`.

## License

Apache License 2.0. See [LICENSE](LICENSE).
