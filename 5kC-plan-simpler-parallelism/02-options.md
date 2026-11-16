# Options for increasing NRI parallelism

Each option is tagged **core**, **add-on**, **alternative** or **rejected**,
with the finding (F-numbers in `01-findings.md`) that decides it, and a
pointer to a design document where one was warranted.

## O1. RWMutex instead of Mutex on `Adaptation` — core building block

Requests take a read lock (the plugin list must be stable while a request
walks it); plugin registration, removal and shutdown take the write lock.
This is what lets two requests be inside NRI at once. Alone it is unsafe:
plugins would receive concurrent calls (F4) and answers would be applied in
an arbitrary order (F9–F11). Both are fixed by O2 + O3. Krisztian's branch
does the same. Design: `03-design-ticket-order.md`.

## O2. Per-plugin serialization in one global entry order ("tickets") — core

A request takes a sequence number as it enters and, under the same lock, a
ticket at every plugin it will reach (subscribed, not parallel). A ticket is
a channel chained behind the previous ticket for that plugin. The request
waits for its ticket before calling the plugin and hands it on after the
call returns. Effects:

- a plugin sees one request at a time (F4 preserved),
- every plugin sees requests in the same order (F10),
- the request clock starts after the wait (F5),
- requests that stop early release what they did not use; no holes, no
  arming, no waiter maps.

Experiment: pipelined `(N+P−1)·d`, 0 order inversions, existing test suite
passes. ~200 lines including the experiment switches. Design:
`03-design-ticket-order.md`.

### O2′. Per-plugin mutex without a global order — rejected

Same speed, but plugin-local orders diverge whenever a request skips a
plugin (F10). Measured 30 inversions in 64 requests. Not sound.

## O3. Ordered apply — core (which variant is a decision)

The runtime must apply Update/Evict in the entry order (F9–F11). Variants:

- **O3a. Apply callback** (`CreateContainerAndApply(ctx, req, apply)` and
  the Update/Stop twins), apply runs inside NRI's order. Krisztian's API,
  reused by the prototype. Cannot be forgotten by a caller, no handle to
  release. Needs a small containerd change (O8). **Recommended.**
- **O3b. Return in order** (exit ticket, no API change). Insufficient:
  containerd applies after return on its own goroutine, so return order is
  not apply order (F11). Only usable if the runtime re-serializes the
  apply, which is O8 anyway.
- **O3c. Per-plugin generation numbers on updates** (last-writer-wins by
  plugin and container; stale updates dropped by the runtime). No blocking
  at all, but pushes ordering semantics into every runtime and into the
  protocol. Rejected for now; noted as a future direction if the apply
  wait ever shows up in measurements.

Only Create/Update/Stop take an apply ticket (F8). Notifications never wait
for an apply.

## O4. Plugin declares parallel event handling — add-on, recommended

One bool in `ConfigureResponse` (`parallel_events`, as on Krisztian's
branch) and a stub option `WithParallelEvents()`. A parallel plugin takes no
ticket: it is called as soon as the request reaches it, so the plugin
alone decides its concurrency. Its answers are still applied in entry
order (the apply ticket is per request, not per plugin); what it gives up
is the order of its own successive decisions, which an independent plugin
does not have (F16, F17).

Experiment: all plugins parallel → `P·d` for any N (128×4: 55 ms vs
1.38 s ordered vs 5.36 s main). One cheap plugin parallel beside a slow
ordered one → the slow plugin is the floor (F15); gain only when the
opting-in plugin was a bottleneck. Design: `03-design-parallel-opt-in.md`.

Variants considered:

- **O4a. Per-event mask** ("I handle Post* in parallel, Create in order").
  A policy plugin could opt its cheap notifications out of its own queue.
  Cheap to add later (an `EventMask` instead of a bool); nri-plugins take
  one lock in every handler so there is no gain for them today. Deferred.
- **O4b. "Never returns cross-container updates"** declaration so a request
  touching only such plugins needs no apply ticket. The apply wait is
  cheap (measured ≈ nothing when requests are homogeneous), so this buys
  little. Deferred.

## O5. Event-class differentiation — partly core, partly rejected

- Notifications carry no apply ticket (in O2/O3). **Core.**
- Making notifications asynchronous (fire-and-forget on a per-plugin queue,
  reply never awaited). **Rejected**: `RunPodSandbox` and `StartContainer`
  errors fail the operation (F8), `Post*` ordering vs the next synchronous
  request would need a flush, and the gain is bounded by the slow plugin
  anyway (F15). Revisit only with real-node data showing notifications
  dominate.

## O6. Per-pod or per-container keyed ordering instead of a global order — rejected

Per-container order is already given by the runtime (F13). Per-pod order
is unsound for policy plugins whose Update lists touch containers of other
pods (F9). A keyed order would also need to know which containers a
plugin's answer will touch before the plugin answers.

## O7. Streaming wire protocol — not a prerequisite; deferred

Buys exact wire order and exact clock start (F21), both of which O2 already
provides for the price of not sending k+1 to a plugin before k is answered.
Costs ≈1.6 k lines of new transport plus 16 k of regenerated protobuf and a
second code path in the stub (F19). No throughput difference measured
(F20 vs prototype). Could be added later as a transport optimization
without touching the ordering design, if a measurement on a real node
shows the per-request goroutine wake-ups or the queue-in-runtime matter.

## O8. containerd wrapper: split the lock, adopt the apply callback — core for containerd

Replace `local.Mutex` with: an RWMutex for `syncPlugin` vs requests, a
small mutex for the `l.state` map, per-container atomicity for the
stop-before-remove check-and-act (F13), and use `*AndApply` so
Update/Evict are applied inside NRI's order (F11). Without this, nothing
changes for containerd (F2). CRI-O needs only the NRI change (F3). Design:
`03-design-containerd-wrapper.md`.

## O9. Plugin-side changes (nri-plugins) — outside NRI, noted

Finer locking in the resource manager (e.g. notifications not taking the
allocation lock) or O4a would let a policy plugin shed its cheap events.
Independent of this plan; listed so the floor in F15 is understood as the
plugin's, not NRI's.

## O10. Validators — small decision inside O2

Validators are stateless approvers (F14). Keep them as today (parallel
goroutines within a request) and let different requests' validations
overlap. If a validator plugin also subscribes to events and keeps state,
it already has to lock itself. No ticket for validations.

## O11. Reduce NRI's fixed per-call cost — not worth it

≈150 µs per request per plugin (F6) against plugin times of milliseconds.
Nothing to do here.

## Summary table

| Option | Status | Gain | Size | Risk |
|---|---|---|---|---|
| O1 RWMutex | core | enables all | tiny | none alone |
| O2 tickets (global order) | core | `N·P·d` → `(N+P−1)·d` | ~120 lines | low |
| O2′ per-plugin mutex | rejected | same | tiny | unsound (F10) |
| O3a apply callback | core | correctness | API addition | runtime must adopt |
| O3b return in order | rejected | — | tiny | not actually ordered |
| O3c generation numbers | deferred | removes apply wait | protocol + runtimes | semantics shift |
| O4 parallel opt-in | add-on | `→ P·d` when the slow plugin opts in | 1 proto field + ~20 lines | wrong declaration corrupts plugin |
| O5 async notifications | rejected | small | medium | semantics |
| O6 keyed order | rejected | — | — | unsound |
| O7 streaming | deferred | none measured | ~1.6 k + 16 k generated | complexity |
| O8 containerd wrapper | core (containerd) | required for any gain there | ~100 lines | per-container state |
