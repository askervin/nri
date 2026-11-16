# Recommendation and implementation plan

## Recommendation

Implement **Design A (ticket order)** and **Design B (parallel opt-in)** in
NRI on top of `origin/main`, and **Design C (wrapper lock split +
`*AndApply`)** in containerd. Do **not** make the streaming wire protocol a
prerequisite; keep it as an optional later transport optimization.

Why this and not the branch as it stands:

- Same measured benefit. Create burst 128 × 4 plugins × 10 ms: main 5.36 s,
  ticket order 1.38 s, branch 1.38 s (its own matrix: 32×4 367 ms vs
  369 ms here). Parallel opt-in: 55 ms vs 51 ms. See
  `03-design-ticket-order.md`.
- One tenth of the core code (≈330 vs ≈3360 non-test lines, no new wire
  protocol, no regenerated protobuf beyond one bool field), with the same
  correctness property (common order at every plugin, applies in that
  order) and the same "clock starts when the plugin gets the request"
  property.
- Everything the branch adds beyond that (exact dequeue-time clock, in-order
  placement of unsolicited updates, streaming) can be layered on later
  without changing the ordering design.
- The sound-vs-unsound line is clear and demonstrated by a test: a
  per-plugin mutex alone produces order inversions; a common entry order
  does not. Reviewers can see why each piece exists.

What has to be true for the gain to appear on a node: at least two plugins
are registered, or one plugin opts into parallel handling. With a single
ordered plugin the plugin itself is the floor (F15) and NRI cannot help;
that should be said in the PR so expectations are right.

## Reuse from Krisztian's branch

Take:

- the `*AndApply` entry points, their names and documentation (the
  "two rules apply is held to" text);
- `parallel_events` in `ConfigureResponse`, `stub.WithParallelEvents()`
  with its warning text, and the log line when a plugin declares it;
- `removeClosed` (take the write lock only when a plugin actually closed);
- the in-process scalability tests (`TestScalabilityMatrix`,
  `TestScalabilityOptInMatrix`, `TestScalabilityOptInBesideAnOrderedPlugin`,
  `TestScalabilityPathological`, `TestScalabilityBurstDoesNotExpireQueuedRequests`,
  `TestApplyRunsInArrivalOrder`) — they call only `*AndApply` and the stub,
  so they port with no transport dependency; accept `apply == nil` as
  "no apply" as the branch does;
- the `nri-test` scale suite (`-nri-test.scale-*`, `-nri-test.scale-parallel-plugins`)
  for real-node validation; it is independent of the transport.

Leave for later (separate decision, after real-node numbers):
`stream.go`, `stub/stream.go`, `sequence.go`, `apply.go`, the
`StreamingRuntime` service, placing unsolicited updates in the order.

## Work items

### PR 1 — NRI: ticket order (Design A)

Repo `containerd/nri`, base `main`.

1. `pkg/adaptation/order.go`: `order`, `ticket`, `turn` (production
   version of `prototype/order.go` without the `NRI_EXP_*` modes; ~120
   lines). `enter` skips plugins with `parallel` set (field lands in PR 2;
   until then always false).
2. `pkg/adaptation/adaptation.go`: `sync.Mutex` → `sync.RWMutex` + `order`
   field; `relay` and `notify` helpers; the ten notification handlers call
   `notify`; `CreateContainerAndApply`, `UpdateContainerAndApply`,
   `StopContainerAndApply` with the apply ticket; `CreateContainer`,
   `UpdateContainer`, `StopContainer` kept, implemented on top; `Stop` and
   plugin add/remove under the write lock; `removeClosed`; `order.forget`
   for closed plugins. Start with `prototype/0001-adaptation-ticket-order.patch`.
3. `pkg/adaptation/plugin.go`: `parallel bool` field (set in PR 2).
4. Tests (`pkg/adaptation`):
   - ordering invariants: 0 inversions in a mixed create/stop burst with
     a skipped plugin (`TestExpOrderInversions`), per-container order,
     apply runs in arrival order (port `TestApplyRunsInArrivalOrder`);
   - robustness: failing plugin mid-chain releases turns; context
     cancelled while queued returns and releases; plugin closes mid-burst;
     plugin registers mid-burst (its chain starts at the next request);
     `Stop()` during a burst; apply callback returning an error does not
     stall the order; apply callback panicking does not stall the order;
   - timeout: queued requests do not expire (`TestExpBurstDoesNotExpireQueuedRequests`);
   - measurement (log only): the matrix tests above.
5. Docs: README section "Ordering and concurrency": what is guaranteed
   (per-plugin serial delivery in one order; applies in that order via
   `*AndApply`; per-container order comes from the runtime), what is not
   (unsolicited updates are applied as they arrive), and the two rules for
   `apply` (must not call back into NRI; must not block on a lock a CRI
   caller holds while inside NRI).

Estimated size: ~400 lines code, ~600 lines tests.

### PR 2 — NRI: parallel opt-in (Design B)

1. `pkg/api/api.proto`: `bool parallel_events = 1` in `ConfigureResponse`;
   regenerate.
2. `pkg/stub/stub.go`: `WithParallelEvents()` option, set in the configure
   reply; doc comment with the warning.
3. `pkg/adaptation/plugin.go`: store `rpl.GetParallelEvents()` in
   `configure`, log it. `order.enter` already skips it.
4. Tests: port `TestScalabilityOptInMatrix` and
   `TestScalabilityOptInBesideAnOrderedPlugin`; a parallel plugin sees
   overlapping handlers; its answers still apply in request order
   (port `TestAParallelPluginsAnswersApplyInRequestOrder`).
5. Docs: plugin author guidance — who should set it (annotation-driven
   adjusters), who must not (resource policies).

Estimated size: ~60 lines code + generated, ~300 lines tests.

PR 1 and PR 2 can be one PR if reviewers prefer; they are separable so
that the protocol change can be discussed on its own.

### PR 3 — containerd: wrapper lock split and `*AndApply` (Design C)

Repo `containerd/containerd`, after an NRI release containing PR 1 (and
PR 2 for the field, not required by the wrapper).

1. `internal/nri/nri.go`: `local.Mutex` → `RWMutex` (write in `syncPlugin`
   and `Stop`, read in requests) + `stateMu` for the state map +
   per-container keyed lock for `stopContainer`/`RemoveContainer`
   check-and-act. Use `CreateContainerAndApply`/`UpdateContainerAndApply`/
   `StopContainerAndApply`; apply Updates inside the callback; perform
   Evictions after it returns (an eviction stops the container and waits
   for it to exit, which must not hold up every other request's apply; the
   exit monitor then issues NRI's `StopContainer` asynchronously as today).
   `updateFromPlugin` takes only the read lock. Details in
   `03-design-containerd-wrapper.md`.
2. Tests: unit tests for the state map under concurrency (stop exactly
   once with concurrent exit notification and remove), and a CRI
   integration test or the `nri-test` scale run.

Estimated size: ~120 lines.

### PR 4 — nri-plugins: opt in where safe

`memory-qos` and other annotation-only adjusters add
`stub.WithParallelEvents()`. Resource policies stay ordered. Optional:
evaluate whether the resource manager can drop `m.Lock()` from pure
notification handlers (independent of NRI).

### Validation on a real node

Before PR 1 merges, run Krisztian's `nri-test` scale suite against
containerd built with the branch of PR 1 + PR 3:

```
bash hack/run-nri-test.sh '' -test.run=TestScalability \
  -nri-test.scale-replicas=50 -nri-test.scale-plugins=3 \
  -nri-test.scale-delay='CreateContainer=20ms,StopContainer=20ms'
# then with the cheap plugins opted in:
bash hack/run-nri-test.sh '' -test.run=TestScalability ... \
  -nri-test.scale-parallel-plugins=2,3
```

Record before/after for: last pod ready time, per-pod latency
distribution, and whether any plugin request timed out. Those numbers go
into the PR descriptions.

## Order of work and dependencies

```
PR 1 (NRI tickets) ──┬──> NRI release ──> PR 3 (containerd wrapper) ──> node validation
PR 2 (NRI opt-in)  ──┘                 └─> PR 4 (nri-plugins opt-in)
```

PR 1 alone benefits CRI-O immediately (no wrapper lock there, F3) and
containerd only after PR 3.

## Risks and how they are covered

| Risk | Covered by |
|---|---|
| A plugin that assumed "no validation while I handle an event" | documented; validators are approve/refuse (F14); the default validator is builtin |
| Plugin registration during a long burst stalls new requests until the pipeline drains (RWMutex writer preference) | same as main today; registration is rare; `removeClosed` only takes the write lock when needed |
| Runtime forgets to use `*AndApply` | old API still works and is correct for Adjust; only cross-request Update order is weaker, which is documented |
| Apply callback blocks the order (slow eviction) or re-enters NRI | design rule; containerd applies only Updates inside the callback and evicts after it; `applyUpdates` goes through the internal `updateContainerResources`, not the CRI handler, so it does not re-enter NRI |
| Wrong `parallel_events` declaration | plugin's own problem by contract; logged at configure time |
| Unsolicited update races an in-flight answer | unchanged from main; nri-plugins already re-push (F12) |

## Follow-ups that are explicitly not part of this plan

- Streaming transport (O7): revisit only if real-node measurement shows
  the queue-in-runtime or goroutine wake-ups matter.
- Placing unsolicited updates in the order (branch commit 0eab4e4).
- Per-event parallel mask (O4a), generation-numbered updates (O3c),
  asynchronous notifications (O5).
