# Design A: ticket order (RWMutex + per-plugin tickets in one entry order + ordered apply)

Options O1 + O2 + O3a from `02-options.md`. This is the core change. It
replaces the global lock with the three things the lock was actually
providing (F1, F4, F9–F11 in `01-findings.md`), each as its own small
mechanism:

| the lock gave | replaced by |
|---|---|
| a stable plugin list during a request | `sync.RWMutex`: read-held per request, write-held to add/remove plugins and to stop |
| one request at a time per plugin, same order at every plugin | a ticket per (request, plugin), chained behind the previous ticket for that plugin, handed out in entry order |
| answers applied in request order | an apply ticket per Create/Update/Stop request, chained behind the previous apply ticket; the runtime's apply runs inside it (`*AndApply` callback) |

Nothing is held while a plugin thinks except the ticket that serializes that
plugin, so requests pipeline through the plugin chain.

## Mechanism

```go
type order struct {
    mu    sync.Mutex
    seq   uint64
    tails map[*plugin]chan struct{} // done channel of the last ticket per plugin
    apply chan struct{}             // done channel of the last apply ticket
}

type ticket struct {
    prev <-chan struct{} // closed when the predecessor is done; nil if none
    done chan struct{}
}
```

`enter(plugins, event, applies)` runs under `order.mu`: `seq++`; for every
live plugin subscribed to `event` that did not declare parallel handling,
make a ticket whose `prev` is the plugin's current tail and make its `done`
the new tail; if `applies`, do the same on the apply chain. Return the
`turn` (map plugin → ticket, apply ticket).

The request then walks the plugin list as today:

```go
for _, p := range r.plugins {
    if p.isClosed() { continue }
    if err := t.wait(ctx, p); err != nil { return err }   // <-prev or ctx.Done()
    err := fn(p)                                          // the existing relay call
    t.done(p)                                             // close(done)
    if err != nil { return err }
}
...validators...
if err := t.waitApply(ctx); err != nil { return err }
return apply(rpl)                                         // deferred t.release() closes apply.done
```

`t.release()` is deferred by every request and closes every `done` the
request still holds, so a request that fails at plugin 2 of 4, or whose
context is cancelled while waiting, hands on the turns at plugins 3 and 4
and at the apply.

That is the whole design. `order.go` in `prototype/` is 216 lines including
the three experiment modes and comments; the production version is about
120. `adaptation.go` shrinks: the thirteen handlers become a `notify`
helper plus three `*AndApply` functions (+190/−184 lines net in the
prototype, most of it the shared helpers).

### Why there are no holes to announce

Krisztian's gate (`sequence.go`) needs every number announced to every
consumer, because a consumer only knows "next is 7" and must be told when 7
is never coming. Chained tickets avoid the question: a ticket exists only
for a plugin the request will reach, so a plugin's chain contains exactly
the requests it will see, in entry order. A plugin that joins mid-way gets
tickets only for later requests (its tail starts empty). A plugin that
closes has its tail forgotten in `removeClosedPlugins`; requests still
holding its tickets skip the closed plugin and release them.

### Why it cannot deadlock

Every wait is on a lower sequence number's `done`, and every `done` is
closed by a request that itself only waits on lower numbers, after a plugin
call that is bounded by the plugin request timeout. There is no cycle. The
write lock (plugin registration, `Stop`) waits for in-flight readers, which
all terminate for the same reason, exactly as on main.

### Properties kept

- **Per-plugin serial delivery, common order (F4, F10).** Measured: 0
  inversions between any plugin's processing order and the apply order in
  the mixed create/stop burst (`TestExpOrderInversions`); the per-plugin
  mutex variant of the same code shows 30.
- **Request clock starts when the plugin gets the request (F5).** The
  per-call timeout context is created after `t.wait`, where it is today.
  Measured: 50 queued creates × 10 ms with a 200 ms request timeout, 0
  failures (`TestExpBurstDoesNotExpireQueuedRequests`), same as main.
- **Per-container lifecycle order (F13).** `TestExpSameContainerOrder`
  holds for 32 containers × 2 plugins.
- **Validators** run as today (goroutines within a request, no ticket).
- **Unsolicited `UpdateContainers`** applied as they arrive (F12), exactly
  main's semantics. Krisztian's placing of such updates in the order is a
  possible later refinement; not needed for parallelism.
- The existing `pkg/adaptation` test suite passes unchanged on the
  prototype.

### Property gained

A request whose context is cancelled while queued returns at once and hands
its turns on (`TestExpCancelledWaiter`: 5 ms on the prototype, 91 ms on
main where the lock wait ignores the context). The requests behind it are
unaffected.

### Property changed

Return order of two concurrent requests is no longer the apply order unless
the runtime uses `*AndApply`. The plain `CreateContainer` etc. are kept and
implemented on top of `*AndApply` with an apply that only captures the
response, so an unmodified runtime still works and is still correct for
Adjust; it only loses the cross-request ordering of Update/Evict that it
never had a contract for. containerd should move to `*AndApply`
(`03-design-containerd-wrapper.md`); CRI-O needs nothing (F3).

## Benchmark results

All runs: in-process `Adaptation`, real stub plugins over a unix socket,
plugins sleep `d` per event. Raw output under `prototype/results-*.txt`.
Repeat with:

```
cd ~/github.com/containerd/nri-exp-main      # origin/main + prototype
go test ./pkg/adaptation -run TestExp -v
cd ~/github.com/containerd/nri-exp-base      # origin/main + rig only
go test ./pkg/adaptation -run TestExp -v
```

### Concurrent CreateContainer burst, d = 10 ms

| N | P | main (`N·P·d`) | ticket order | `(N+P−1)·d` | Krisztian's branch | all parallel (`P·d`) |
|---|---|---|---|---|---|---|
| 8 | 1 | 83 ms | 83 ms | 80 | 83 ms | 11 ms |
| 32 | 1 | 336 ms | 335 ms | 320 | 330 ms | 16 ms |
| 128 | 1 | 1.341 s | 1.344 s | 1280 | – | 15 ms |
| 8 | 2 | 167 ms | 94 ms | 90 | 94 ms | 21 ms |
| 32 | 2 | 669 ms | 345 ms | 330 | 345 ms | 23 ms |
| 128 | 2 | 2.686 s | 1.352 s | 1290 | – | 32 ms |
| 8 | 4 | 335 ms | 115 ms | 110 | 116 ms | 46 ms |
| 32 | 4 | 1.339 s | 369 ms | 350 | 367 ms | 46 ms |
| 128 | 4 | 5.362 s | 1.382 s | 1310 | – | 55 ms |

The ticket order is the pipeline bound to within 5 %, and identical to the
streaming branch. With one plugin there is nothing to gain (F15). With four
plugins the 128-create burst goes from 5.4 s to 1.4 s; the last create's
latency drops accordingly.

### Fixed cost (d = 0)

About 150 µs per request per plugin on main, 64–150 µs with tickets (the
calls to different plugins overlap). Irrelevant against plugin work.

### Mixed lifecycle burst (N pods × 5 up-events, then × 4 down-events)

Plugin 0 is policy-like: 10 ms on `CreateContainer` and `StopContainer`,
0 otherwise, subscribed to everything. Plugin 1 is adjuster-like: 1 ms on
each of the 9 events.

| N | main up/down | ticket order | per-plugin mutex | ticket order, plugin 1 parallel | everything parallel |
|---|---|---|---|---|---|
| 8 | 142 / 127 ms | 122 / 118 | 120 / 118 | 94 / 91 | 19 / 18 |
| 32 | 553 / 501 ms | 475 / 460 | 467 / 460 | 371 / 352 | 21 / 24 |
| 64 | 1.109 / 0.990 s | 936 / 920 | 933 / 911 | 730 / 708 | 33 / 26 |

Reading: when the slow plugin subscribes to every event, its own FIFO is
the floor (64 × 10 ms creates plus 256 cheap round trips ≈ 700 ms), and
the cheap plugin's work is issued only after the creates drain, so
pipelining buys little here (936 vs 1109 ms) and the choice of per-plugin
order mechanism buys nothing (936 vs 933). The parallel opt-in for the
cheap plugin removes its serialized tail (→ 730 ms, the floor). A real
node issues events as kubelet gets to them rather than all at once, so
the all-at-once figure is the worst case for the FIFO effect.

## Comparison with the streaming branch

| | ticket order (this) | branch (`sequence.go` + `apply.go` + streams) |
|---|---|---|
| wire protocol | unchanged | new bidirectional stream service, 16 k generated lines |
| stub | unchanged (opt-in option only) | second transport, `stub/stream.go` 729 lines |
| ordering state | two maps of channels | gate with waiters, holes, arming, serving marker; attachments below numbers |
| burst time | `(N+P−1)·d` | `(N+P−1)·d` |
| timeout starts | after the ticket wait | when the plugin dequeues from its stream |
| unsolicited updates | as they arrive (main semantics) | placed below the lowest owed number |
| plugin on an old NRI | unaffected | unaffected (falls back to unary) |
| non-test core lines | ≈ 330 | ≈ 3360 |

The two things the stream adds (exact dequeue-time clock, in-order
placement of unsolicited updates) are refinements that can be added to the
ticket design later without changing it; neither affects throughput.

## Open decisions

1. **Should the per-plugin wait honour the caller's context?** The prototype
   does (cheap, and it is what a CRI deadline means). Main does not. Keep.
2. **Validators of a plugin that is also an event subscriber** could now
   interleave differently (a validation of request 2 may reach the plugin
   while it handles request 3). Validators are approve/refuse only (F14);
   document it, do not serialize.
3. **Unsolicited update ordering** (F12): keep main's semantics in the first
   PR; consider Krisztian's placement as a follow-up if a plugin author
   asks for it.
