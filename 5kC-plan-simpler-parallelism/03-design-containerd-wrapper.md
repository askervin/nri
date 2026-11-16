# Design C: containerd wrapper — split the lock, adopt the ordered apply

Option O8 from `02-options.md`. Without this, Design A changes nothing for
containerd, because `internal/nri/nri.go` holds its own global mutex around
every call into NRI (F2 in `01-findings.md`). CRI-O has no such wrapper
(F3) and needs nothing here.

## What the wrapper's lock protects today

`type local struct { sync.Mutex; cfg; nri *nri.Adaptation; state map[string]State }`

1. The call into `nri.Adaptation` (redundant with NRI's own lock).
2. Applying the response's `Update` and `Evict` after NRI returns
   (`applyUpdates`, `evictContainers`) — this is what made apply order equal
   request order (F11).
3. The `state` map: `setState`, `needsStopping`, `needsRemoval`, used so
   that `stopContainer` runs exactly once per container whether triggered by
   CRI `StopContainer`, by `RemoveContainer` (stop first if needed) or by the
   asynchronous exit notification (`NotifyContainerExit` → goroutine).
4. `syncPlugin`: a consistent snapshot of pods/containers plus the state map
   while a new plugin synchronizes.
5. `updateFromPlugin`: unsolicited updates, serialized against requests.

## Proposed shape

```go
type local struct {
    cfg   *Config
    nri   *nri.Adaptation

    // sync: write-held by syncPlugin and Stop, read-held by every request,
    // so a joining plugin sees a stable snapshot and a stable state map.
    sync  sync.RWMutex

    // stateMu guards state. Held for map access only.
    stateMu sync.Mutex
    state   map[string]State

    // ctrs serializes the stop/remove check-and-act per container id, so
    // StopContainer, RemoveContainer and the exit notification for one
    // container cannot interleave. A small striped lock is enough.
    ctrs keyedLocks
}
```

Per request:

```go
func (l *local) CreateContainer(ctx, pod, ctr) (*nri.ContainerAdjustment, error) {
    l.sync.RLock(); defer l.sync.RUnlock()

    var adjust *nri.ContainerAdjustment
    var evict  []*nri.ContainerEviction
    req := &nri.CreateContainerRequest{...}
    err := l.nri.CreateContainerAndApply(ctx, req, func(rpl *nri.CreateContainerResponse) error {
        adjust, evict = rpl.Adjust, rpl.Evict
        if _, err := l.applyUpdates(ctx, rpl.Update); err != nil {
            log...("pre-create update failed")      // as today: logged, not fatal
        }
        return nil
    })
    l.setState(req.Container.Id, Created)
    if err != nil { return nil, err }

    // Outside the ordered apply: eviction stops a container and waits for
    // it (stopContainerRetryOnConnectionClosed), which must not hold up the
    // apply order of every other request; the exit monitor then issues the
    // NRI StopContainer for it asynchronously, as it does today.
    if _, err := l.evictContainers(ctx, evict); err != nil { log...("pre-create eviction failed") }
    return adjust, nil
}
```

`UpdateContainer` and `StopContainer` likewise with
`UpdateContainerAndApply`/`StopContainerAndApply`. Notifications
(`RunPodSandbox`, `PostCreateContainer`, …) just take `sync.RLock` and call
NRI.

`stopContainer`/`RemoveContainer`:

```go
func (l *local) stopContainer(ctx, pod, ctr) error {
    unlock := l.ctrs.lock(ctr.GetID()); defer unlock()
    if !l.needsStopping(ctr.GetID()) { return nil }
    err := l.nri.StopContainerAndApply(ctx, req, func(rpl) error { l.applyUpdates(ctx, rpl.Update); return nil })
    l.setState(ctr.GetID(), Stopped)
    return err
}
```

`RemoveContainer` takes the same per-container lock around
`needsRemoval` → `stopContainer` (re-entrant need avoided by an unlocked
inner variant) → `nri.RemoveContainer` → `setState(Removed)`.
`NotifyContainerExit` keeps its goroutine and calls `stopContainer`.

`syncPlugin` takes `sync.Lock()`; `updateFromPlugin` takes `sync.RLock()`
and applies directly (F12: as-it-arrives semantics, unchanged).

## Rules the apply callback must obey (from Design A)

- It must not call back into NRI. `applyUpdates` →
  `domains.updateContainers` → `criImplementation.UpdateContainerResources`
  → `criService.updateContainerResources` (internal, not the CRI handler)
  does not. Evictions are kept outside the callback for the reason above.
- It must not block on a lock a CRI handler can hold while inside NRI.
  `l.sync.RLock` is held by the caller already; `stateMu` and `ctrs` are
  never held across an NRI call by anything else except the per-container
  lock of the *same* container, which no other request holds (the runtime
  issues one container's lifecycle calls sequentially, F13).

## What stays the same for CRI handlers

`BlockPluginSync()` around every CRI operation (an RLock on NRI's
`syncLock`) is unchanged and already allows concurrency. No other lock in
`internal/cri/server` is on the NRI path (F23).

## Size and risk

About 120 lines changed in `internal/nri/nri.go` plus a 20-line keyed
lock. The one behavioural subtlety is item 3 above (exactly-once stop);
a unit test with a concurrent exit notification and remove for the same
container pins it. Requires a `containerd/nri` release containing Design A
(`go.mod` currently pins v0.12.5).
