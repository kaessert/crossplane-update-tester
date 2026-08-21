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
update-tester validate <manifest.yaml> --types-file <types.go> [--controller-dir <dir>]
update-tester expect-skeleton <types.go> --kind <Kind> --field <field>
update-tester check-external-name-prefix <manifest.yaml> [--timeout 30]
update-tester resolve-recover <manifest.yaml> [--timeout 120]
update-tester roundtrip-diff <m1.yaml,m2.yaml,...> [--root <dir>] [--timeout 30]
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
9. **Inverts the verdict for a `knownDefect:` entry.** Steps 1–7 run exactly
   as above, but the pass/fail reading is flipped: non-convergence is this
   entry's EXPECTED outcome and is reported as `KNOWN-DEFECT`, neither a
   pass nor a failure. If the field actually converges with positive
   evidence, that is reported as `KNOWN-DEFECT CONVERGED` and FAILS the run
   hard, naming the ticket ID and instructing the reader to delete the token
   — see "`crossplane.io/update-test`" below.

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
- **Shortened window for `knownDefect` entries.** An unfixed `knownDefect`
  entry is, by construction, expected to spend its ENTIRE convergence window
  failing to converge on every single run — unlike an ordinary entry, which
  typically converges and returns early. Running it at `--timeout`'s full
  value would tax every invocation that carries one for no benefit, so `run`
  narrows the window to a quarter of `--timeout` (floored at 15s, so a short
  `--timeout` still leaves room for the two forced reconciles every field
  test drives) for a `knownDefect` entry only. The trade-off: a defect that
  would genuinely converge only in the last quarter of the full window is
  misreported as `KNOWN-DEFECT` rather than caught as fixed on that
  particular run — an acceptable false negative, since the entry is
  re-evaluated on every subsequent run and the ticket, not this tool, is
  what proves the fix.

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
the manifest's annotation covers it:

- `tested` / `skipped` — the field appears in the annotation.
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

and the command exits non-zero. This check runs entirely offline — the
manifest's own `spec.forProvider` and `crossplane.io/update-test` annotation
are enough — so it applies even without `--types-file`.

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
| `value` | The value to patch in. Required unless `skip` is set. |
| `expect` | Optional. The value expected in `status.atProvider` when the backend normalises what it stores. Defaults to `value`. |
| `skip` | Optional. A reason for not testing this field. The entry is reported as `SKIPPED` and still counts as coverage for `validate`. Mutually exclusive with `knownDefect`. |
| `clear` | Optional. A list of OTHER top-level `spec.forProvider` field names nulled in the SAME merge patch that sets `field`'s value, so a union modeled as separate top-level fields switches arms atomically. Only valid when `field` itself is top-level (not dotted); a dotted entry, or one naming `field` itself, is rejected at parse time. |
| `knownDefect` | Optional. The ID of the ticket tracking a real defect that keeps this field's update path from converging. Unlike `skip`, the entry RUNS: `value` is required, the patch is applied exactly as normal, and non-convergence is reported as `KNOWN-DEFECT` — expected, not a failure. If the field DOES converge, that FAILS the run hard as `KNOWN-DEFECT CONVERGED`, naming the ticket and telling the reader to delete the token. Mutually exclusive with `skip`. The value must be non-empty, contain no whitespace, not be a placeholder (`TODO`, `TBD`, `n/a`, etc.), and be at least 6 characters — reject anything that cannot be followed back to an actual ticket. Also rejected if the same top-level field name appears in this manifest's own `ignore-fields:` set (dead config — see `ignore-fields:` below). |

None of `converge-skip: <reason>`, `assert-unchanged: <fields>` or
`ignore-fields: <fields>` is valid YAML as a sibling of top-level sequence
items, so all three lines are extracted before the rest of the block is
parsed as a sequence. Each must be unindented.

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

Worked example:

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
