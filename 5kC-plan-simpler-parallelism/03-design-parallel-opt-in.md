# Design B: plugin declares parallel event handling

Option O4 from `02-options.md`. An add-on to Design A: it is what turns
the pipeline bound `(N+P−1)·d` into `P·d` for plugins that do not need
ordering, and it is what lets a cheap adjuster stop queueing behind a slow
policy plugin.

## Protocol

One field, as on Krisztian's branch (`api.proto`, `ConfigureResponse`):

```proto
message ConfigureResponse {
  // Whether the plugin handles the events it subscribes to in parallel,
  // rather than one at a time in the order the runtime dispatched them.
  bool parallel_events = 1;
  int32 events = 2;
  ...
}
```

Stub: `stub.WithParallelEvents()` sets it in the configure reply. Runtime:
`plugin.configure` stores it; `order.enter` hands no ticket to such a
plugin. That is the entire runtime change (two lines in the prototype,
where the flag comes from an environment variable instead of the reply).

An old runtime ignores the field (proto3 unknown field) and keeps
serializing the plugin: safe. An old plugin never sets it: unchanged
behaviour.

## Semantics (what the plugin promises, what it keeps)

Promise: every handler decides from its request alone, or protects its own
shared state. The runtime cannot check this (F17); `WithParallelEvents`
carries the warning text from Krisztian's branch.

Keeps: the runtime still applies the plugin's answers in request order
(the apply ticket is per request). Loses: the order of the plugin's own
successive decisions, and ordering of its unsolicited updates against its
answers. For a plugin that decides from the request alone there is nothing
to lose.

## Measured effect

Create burst, d = 10 ms, every plugin parallel (`prototype/results-prototype.txt`):

| N | P | ordered | all parallel |
|---|---|---|---|
| 32 | 1 | 335 ms | 16 ms |
| 128 | 2 | 1.352 s | 32 ms |
| 128 | 4 | 1.382 s | 55 ms |

Mixed lifecycle burst, slow policy-like plugin 0 ordered, cheap plugin 1
parallel (`prototype/results-prototype-2.txt`): 936 → 730 ms at N = 64,
where ≈ 700 ms is plugin 0's own work. Krisztian's
`TestScalabilityOptInBesideAnOrderedPlugin` makes the same point from the
other side: the gain is `max(dA, dB)/dB`; a parallel plugin beside a slower
ordered one gains nothing. In a deployment of nri-plugins (policy, F15)
plus memory-qos, the policy plugin is the floor and this opt-in removes
memory-qos from the critical path.

## Variants considered (deferred)

- **Per-event mask** instead of a bool: a policy plugin could declare its
  `Post*`/`Start`/`Remove` handlers parallel and keep `Create`/`Stop`
  ordered. nri-plugins take `m.Lock()` in every handler today, so there is
  no gain until the plugin changes too. Proto-compatible to add later
  (`int32 parallel_events_mask`). Not in the first PR.
- **"Never returns Update/Evict"** declaration, so a request that reaches
  only such plugins takes no apply ticket. The apply wait costs nothing
  measurable in homogeneous bursts; skip.

## Who should set it

memory-qos and similar annotation-driven adjusters: yes. Resource policies
(topology-aware, balloons, template): no. Validators: not applicable (no
ticket anyway). The default validator is builtin and unaffected.
