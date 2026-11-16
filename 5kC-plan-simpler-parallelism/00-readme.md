# Simpler parallelism for NRI — research results

Task: `../5kC-task-simpler-parallelism.txt`. Study NRI `origin/main`,
Krisztian's branch `proto/nri-test+streaming-wire-protocol`, and
containerd; find options for letting NRI process container lifecycle
events in parallel; run small experiments; recommend a simple design.

Date: 2026-11-15. Repos and commits are listed at the top of `01-findings.md`.

## Files

| File | Contents |
|---|---|
| `01-findings.md` | Facts (F1–F23): where serialization happens (two stacked global locks: NRI's and containerd's wrapper's), what ordering is actually required (per-plugin decision order, common across plugins, applied in that order), cost model, plugin-side facts, what the streaming branch does and measures, kubelet burst shape. |
| `02-options.md` | Eleven options (O1–O11), each tagged core / add-on / rejected / deferred with the finding that decides it, and a summary table. |
| `03-design-ticket-order.md` | **Design A (recommended core):** RWMutex + a ticket per (request, plugin) chained in entry order + ordered apply through `*AndApply` callbacks. Mechanism, why no holes, why no deadlock, properties kept/gained/changed, benchmark tables (main vs prototype vs branch), comparison with the streaming branch. |
| `03-design-parallel-opt-in.md` | **Design B (recommended add-on):** one `parallel_events` bool in the configure reply; the plugin takes no ticket. Measured `(N+P−1)·d → P·d`. |
| `03-design-containerd-wrapper.md` | **Design C (required for containerd):** split `internal/nri/nri.go`'s global lock, adopt `*AndApply`, keep evictions outside the ordered apply. |
| `04-plan.md` | Recommendation, what to reuse from Krisztian's branch, four PRs (NRI tickets, NRI opt-in, containerd wrapper, nri-plugins opt-in), real-node validation, risks, explicit non-goals. |
| `prototype/` | Evidence. See below. |

## Prototype and raw results (`prototype/`)

| File | What |
|---|---|
| `order.go` | The ticket mechanism (216 lines with three experiment modes selected by `NRI_EXP_ORDER=global\|mutex\|none` and `NRI_EXP_PARALLEL_PLUGINS=all\|name,...`). |
| `0001-adaptation-ticket-order.patch` | Diff of `adaptation.go` and `plugin.go` against `origin/main` (+190/−184, +13/−4). |
| `exp_parallel_test.go` | Measurement and invariant rig: create matrix, fixed cost, mixed lifecycle burst, order inversions, per-container order, queued-request timeout, cancelled waiter. Compiles against unmodified main too. |
| `results-baseline-main.txt` | Rig on unmodified `origin/main`. |
| `results-prototype.txt` | Rig on the prototype in all modes, plus the existing `pkg/adaptation` suite passing. |
| `results-prototype-2.txt` | Lifecycle burst with the cheap plugin opted in, mutex mode, timeout and cancellation tests, baseline comparison. |
| `results-klihub-branch.txt` | Krisztian's own `TestScalability*` on his branch, same machine. |

Headline numbers (128 concurrent creates, 4 plugins, 10 ms each):
main 5.36 s; ticket order 1.38 s; all plugins parallel 55 ms; Krisztian's
branch at 32×4: 367 ms vs 369 ms here. Order inversions in a mixed burst:
per-plugin mutex 30, common entry order 0.

## Reproducing

Two git worktrees of this repository were created for the experiments and
left in place:

- `~/github.com/containerd/nri-exp-main` — `origin/main` + prototype + rig
- `~/github.com/containerd/nri-exp-base` — `origin/main` + rig only

```
cd ~/github.com/containerd/nri-exp-main
go test ./pkg/adaptation -run TestExp -v                       # ticket order
NRI_EXP_ORDER=mutex NRI_EXP_EXPECT_INVERSIONS=1 go test ./pkg/adaptation -run TestExpOrderInversions -v
NRI_EXP_PARALLEL_PLUGINS=all go test ./pkg/adaptation -run TestExpCreateMatrix -v
go test ./pkg/adaptation/...                                   # existing suite
cd ~/github.com/containerd/nri-exp-base
go test ./pkg/adaptation -run TestExp -v                       # baseline
```

To remove the worktrees: `git worktree remove --force
~/github.com/containerd/nri-exp-main` and the same for `nri-exp-base`.
