# Findings: facts relevant for NRI plugin parallelism

Sources: `containerd/nri` at `origin/main` (d817e4e), the branch
`proto/nri-test+streaming-wire-protocol` (fda220e), containerd at
`~/github.com/containerd/containerd` (5ae3ecba0), `containers/nri-plugins`
(832d843ba), kubelet source, CRI-O `server/nri-api.go` (GitHub main), and
the experiments under `prototype/` (see `03-design-ticket-order.md`).

## A. Where serialization happens today

**F1. One global lock in NRI.** `Adaptation` embeds `sync.Mutex`
(`pkg/adaptation/adaptation.go`). All 13 request entry points
(`RunPodSandbox` … `RemoveContainer`) do `r.Lock(); defer r.Unlock()` for
the whole request, including every plugin round trip. One request at a
time, across all plugins, all pods and all containers. Only validators run
concurrently, and only within one request.

**F2. A second global lock in containerd.** containerd's wrapper
`internal/nri/nri.go` (`type local struct { sync.Mutex ... }`) takes its own
mutex in every method around the call into `nri.Adaptation`, around applying
the returned `Update`/`Evict` lists (`applyUpdates`), and around its
per-container state map (`l.state`, used by `needsStopping`/`needsRemoval`).
`updateFromPlugin` (unsolicited updates) and `syncPlugin` take it too.
Removing F1 alone changes nothing for containerd; F2 must go with it.

**F3. CRI-O has no wrapper lock.** CRI-O's `server/nri-api.go` (`nriAPI`
struct) holds no mutex and forwards directly to `pkg/adaptation`. CRI-O gets
the benefit of an NRI-side change for free.

**F4. The transport does not serialize.** ttrpc v1.2.7 multiplexes
concurrent calls on one connection (client stream IDs, server goroutine per
request). The stub (`pkg/stub`) has no handler-level lock. If the runtime
sent two requests to a plugin at once, the plugin's handlers would run at
once. Plugins have never been exposed to this because of F1. Compatibility
constraint: the default must stay "one request at a time per plugin".

**F5. The plugin request timeout starts after the lock.** The 2 s default
(`api.DefaultPluginRequestTimeout`) context is created inside
`plugin.createContainer` etc., right before the ttrpc call, i.e. after the
global lock was acquired. Queue wait does not eat the budget. Any new design
must keep "the clock starts when the plugin gets the request", or a long
burst expires queued requests.

**F6. Cost model (measured).** With real stub plugins over unix sockets,
in-process runtime, no plugin work: about 150 µs per request per plugin.
With plugin work `d`, a burst of `N` requests through `P` plugins costs
`N·P·d` on main. Measured 128 creates × 4 plugins × 10 ms = 5.36 s (model
5.12 s). The wire cost is irrelevant next to plugin time.

## B. What a request is, and what ordering it actually needs

**F7. The plugin chain inside one request is inherently sequential.**
Plugins are called in index order, and for `CreateContainer` each plugin's
adjustment is merged into the result *and into the request*
(`result.apply` mutates `req.Container`), so plugin k+1 sees plugin k's
changes. Parallelism can only come from different requests overlapping,
never from calling one request's plugins concurrently.

**F8. Only three requests can answer with changes.** `CreateContainer`
(Adjust for this container, Update/Evict for others), `UpdateContainer`
(Update, including the container itself), `StopContainer` (Update for
others). The other ten are notifications: a plugin can return an error
(`RunPodSandbox`/`StartContainer` errors fail the operation; `Post*`
errors are only logged by containerd) but cannot change anything.

**F9. Adjust needs no cross-request ordering; Update/Evict does.** Adjust
targets the container being created, which no other request touches.
Update/Evict target *other* containers. If plugin P decided "X gets cpuset
A" for request 1 and "X gets cpuset B" for request 2, B must be applied
after A, or P's older decision lands last. This is the single correctness
property that lock removal must preserve, and it is per plugin: what
matters is that each plugin's successive decisions take effect in the order
that plugin made them.

**F10. The order must be the same for all plugins.** If plugin P saw
request 1 before 2 but plugin Q saw 2 before 1, no single apply order is
right for both, because one request's answer merges all plugins' updates.
Hence per-plugin serialization alone is not enough: every plugin has to see
requests in one common order, and answers must be applied in that order.
Experiment (`TestExpOrderInversions`): per-plugin mutex order gave 30
inversions in 64 mixed create/stop requests; one global entry order gave 0.
A plugin that some requests skip (not subscribed, or an earlier plugin
failed the request) is exactly what makes plugin-local orders diverge.

**F11. Today's apply order is the return order.** In containerd the
Create/Stop response's Update/Evict are applied by containerd after NRI
returns, under F2. Return order = global lock order = apply order. Once NRI
returns concurrently, containerd's goroutines could apply out of order
unless the apply is done inside NRI's order (callback) or containerd
re-serializes it.

**F12. Unsolicited updates already race with requests.** A plugin's
`UpdateContainers` is applied as it arrives (via `UpdateFn`, which in
containerd waits for F2, i.e. between requests). The nri-plugins resource
manager re-pushes updates that changed while in flight (`pushOnce`,
`finishPush`) precisely because of this. "Apply as it arrives" is the
semantics plugins already live with. A handler that calls `UpdateContainers`
synchronously before replying deadlocks against F2 on main today.

**F13. Per-container lifecycle order comes from the runtime, not the
lock.** Create→Start→Stop→Remove for one container is sequential because
the runtime issues those calls one after another for that container
(kubelet starts a pod's containers sequentially; each CRI call returns
before the next). Any design keeps it. The exception is containerd's
`NotifyContainerExit`, which runs `stopContainer` in a goroutine; the
`l.state` map makes stop-before-remove exactly-once. That check-and-act
needs per-container atomicity when F2 goes.

**F14. Validators.** `ValidateContainerAdjustment` is already called in
parallel goroutines within a request and only approves/refuses. No
ordering is needed, but a validator would now also see calls from different
requests concurrently.

## C. Plugin-side facts

**F15. Resource policy plugins serialize themselves.** nri-plugins
(`pkg/resmgr/nri.go`) takes the resource-manager lock `m.Lock()` in every
handler. With such a plugin, N requests cost at least `N·d_plugin` whatever
NRI does. What NRI adds today is everything *else* waiting behind that
plugin: other plugins' work, other pods' requests, and the cheap
notification events of all pods. The same holds for a single plugin: with
P = 1 nothing in NRI can help (measured: 32 creates × 1 plugin = 335 ms on
main and on every prototype variant).

**F16. Independent plugins exist.** memory-qos style plugins decide from
the request alone (annotations) and could handle events concurrently. Today
they wait behind policy plugins and behind their own earlier events.

**F17. Independence cannot be verified by NRI.** It has to be a plugin
declaration, and a wrong declaration corrupts the plugin's own state with
no error anywhere. Krisztian's `WithParallelEvents` doc comment says so.

## D. Krisztian's branch (`proto/nri-test+streaming-wire-protocol`)

**F18. What it does.** (a) `sync.RWMutex` on `Adaptation`, one sequence
number per request, and an applier (gate with "holes") that runs applies in
sequence order through new `CreateContainerAndApply`,
`UpdateContainerAndApply`, `StopContainerAndApply` entry points taking a
callback. (b) A bidirectional ttrpc stream per plugin
(`StreamingRuntime.StartEventStream`): the runtime writes requests in
sequence order, the stub answers one at a time in that order. (c)
`parallel_events` bool in `ConfigureResponse` to take a plugin out of the
order. (d) Unsolicited updates placed in the order below the lowest number
the plugin still owes. (e) The request clock starts when the plugin gets
the request.

**F19. Size.** Non-test, non-generated changes to `pkg/adaptation`,
`pkg/stub`, `api.proto`: +3360/−106 lines (`stream.go` 900,
`stub/stream.go` 729, `sequence.go` 413, `apply.go` 321, plus
`adaptation.go`/`plugin.go`/`stub.go`); tests +4365; regenerated protobuf
≈16 k lines.

**F20. Measured on the branch** (`TestScalabilityMatrix`, 10 ms plugins):
ordered ≈ `(N+P−1)·d` (32×4: 367 ms vs 1.28 s on main); all plugins
parallel ≈ `P·d` (51 ms). The prototype in this directory reproduces the
same numbers without streaming (`03-design-ticket-order.md`).

**F21. What streaming buys and does not buy.** It makes the wire order
equal the dispatch order, so request k+1 can sit in the plugin's queue while
the plugin works on k, and the per-request timeout can start exactly when
the plugin dequeues it. It does not buy throughput: a plugin that handles
one request at a time has the same throughput whether request k+1 waits in
the runtime (ticket) or in the plugin's stream queue. Both the wire-order
and the clock-start properties are obtained without a new wire protocol by
not sending k+1 before k is answered and starting the clock after that wait
(F5 already works that way).

## E. Burst shape from kubelet and containerd

**F22. Concurrency = pods in flight.** Kubelet runs a worker goroutine per
pod (`podWorkerLoop`); containers of one pod are started sequentially
(`SyncPod` loop over `ContainersToStart`). So NRI sees up to "pods in
flight" concurrent requests, each pod's events in sequence. Kubelet's CRI
request timeout is 2 min (`RuntimeRequestTimeout`); a pod waiting behind a
long serialized burst can hit it.

**F23. containerd itself is concurrent around NRI.** The CRI server handles
requests concurrently and takes no CRI-wide lock around NRI calls, only
`BlockPluginSync()` (an RLock on NRI's `syncLock`), which already permits
concurrent requests. So after F1 and F2 are fixed there is no further lock
in the path.
