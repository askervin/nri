/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package adaptation_test

// Experiment rig for NRI request parallelism (5kC research).
//
// This file is deliberately self-contained so that it compiles against
// unmodified origin/main (baseline) and against the prototype in this
// worktree. Plugins are real stub-based plugins talking ttrpc over a unix
// socket, so the numbers include the wire.
//
// Nothing here asserts performance; tests print tables. The only assertion
// is in the ordering tests, which check the property the design must keep:
// if a plugin handled request i before request j, then i's answer must be
// put into practice (returned to the runtime) before j's.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	nri "github.com/containerd/nri/pkg/adaptation"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

const expStartupTimeout = 10 * time.Second

// expPlugin is a stub plugin that sleeps a configurable time per event and
// records the order in which it started handling requests.
type expPlugin struct {
	name   string
	idx    string
	stub   stub.Stub
	events []string
	delay  map[string]time.Duration // per event name, "" = default

	synced chan struct{}
	once   sync.Once

	mu      sync.Mutex
	seen    []string // "<event>:<id>" in handling-start order
	inside  int
	overlap int
}

func newExpPlugin(idx int, events []string, delay map[string]time.Duration) *expPlugin {
	return &expPlugin{
		name:   fmt.Sprintf("exp%d", idx),
		idx:    fmt.Sprintf("%02d", idx),
		events: events,
		delay:  delay,
		synced: make(chan struct{}),
	}
}

func (p *expPlugin) delayFor(event string) time.Duration {
	if d, ok := p.delay[event]; ok {
		return d
	}
	return p.delay[""]
}

func (p *expPlugin) handle(event, id string) {
	p.mu.Lock()
	p.seen = append(p.seen, event+":"+id)
	if p.inside > 0 {
		p.overlap++
	}
	p.inside++
	p.mu.Unlock()

	if d := p.delayFor(event); d > 0 {
		time.Sleep(d)
	}

	p.mu.Lock()
	p.inside--
	p.mu.Unlock()
}

func (p *expPlugin) Configure(context.Context, string, string, string) (stub.EventMask, error) {
	return api.MustParseEventMask(p.events...), nil
}

func (p *expPlugin) Synchronize(context.Context, []*api.PodSandbox, []*api.Container) ([]*api.ContainerUpdate, error) {
	p.once.Do(func() { close(p.synced) })
	return nil, nil
}

func (p *expPlugin) RunPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	p.handle("RunPodSandbox", pod.Id)
	return nil
}

func (p *expPlugin) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	p.handle("StopPodSandbox", pod.Id)
	return nil
}

func (p *expPlugin) RemovePodSandbox(_ context.Context, pod *api.PodSandbox) error {
	p.handle("RemovePodSandbox", pod.Id)
	return nil
}

func (p *expPlugin) CreateContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	p.handle("CreateContainer", ctr.Id)
	a := &api.ContainerAdjustment{}
	a.AddAnnotation("nri.exp/"+p.name, ctr.Id)
	return a, nil, nil
}

func (p *expPlugin) PostCreateContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) error {
	p.handle("PostCreateContainer", ctr.Id)
	return nil
}

func (p *expPlugin) StartContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) error {
	p.handle("StartContainer", ctr.Id)
	return nil
}

func (p *expPlugin) PostStartContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) error {
	p.handle("PostStartContainer", ctr.Id)
	return nil
}

func (p *expPlugin) StopContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	p.handle("StopContainer", ctr.Id)
	return nil, nil
}

func (p *expPlugin) RemoveContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) error {
	p.handle("RemoveContainer", ctr.Id)
	return nil
}

func (p *expPlugin) Seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *expPlugin) Overlap() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overlap
}

func (p *expPlugin) start(dir string) error {
	s, err := stub.New(p,
		stub.WithPluginName(p.name),
		stub.WithPluginIdx(p.idx),
		stub.WithSocketPath(filepath.Join(dir, "nri.sock")),
	)
	if err != nil {
		return err
	}
	p.stub = s
	return s.Start(context.Background())
}

func (p *expPlugin) stop() {
	if p.stub != nil {
		p.stub.Stop()
		p.stub.Wait()
	}
}

// expRig is a started Adaptation with plugins attached and synchronized.
type expRig struct {
	runtime *nri.Adaptation
	plugins []*expPlugin
	dir     string

	mu      sync.Mutex
	applied []string // "<event>:<id>" in the order answers came back
}

// record notes that a request's answer has come back to the runtime, which
// is where a runtime would apply it.
func (r *expRig) record(event, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, event+":"+id)
}

func (r *expRig) Applied() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.applied...)
}

type expPluginSpec struct {
	events []string
	delay  map[string]time.Duration
}

var allExpEvents = []string{
	"RunPodSandbox", "StopPodSandbox", "RemovePodSandbox",
	"CreateContainer", "PostCreateContainer", "StartContainer", "PostStartContainer",
	"StopContainer", "RemoveContainer",
}

func startExpRig(t *testing.T, specs []expPluginSpec) *expRig {
	t.Helper()
	logrus.SetLevel(logrus.ErrorLevel)

	dir, err := os.MkdirTemp("", "nri-exp-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "etc", "nri"), 0o755))

	rig := &expRig{dir: dir}

	syncFn := func(ctx context.Context, cb nri.SyncCB) error {
		_, err := cb(ctx, nil, nil)
		return err
	}
	updateFn := func(context.Context, []*nri.ContainerUpdate) ([]*nri.ContainerUpdate, error) {
		return nil, nil
	}

	runtime, err := nri.New("exp-runtime", "0.0.1", syncFn, updateFn,
		nri.WithPluginPath(filepath.Join(dir, "opt", "nri", "plugins")),
		nri.WithPluginConfigPath(filepath.Join(dir, "etc", "nri", "conf.d")),
		nri.WithSocketPath(filepath.Join(dir, "nri.sock")),
	)
	require.NoError(t, err)
	require.NoError(t, runtime.Start())
	t.Cleanup(runtime.Stop)
	rig.runtime = runtime

	for i, s := range specs {
		p := newExpPlugin(i, s.events, s.delay)
		require.NoError(t, p.start(dir))
		t.Cleanup(p.stop)
		rig.plugins = append(rig.plugins, p)
	}
	for _, p := range rig.plugins {
		select {
		case <-p.synced:
		case <-time.After(expStartupTimeout):
			t.Fatalf("plugin %s did not synchronize", p.name)
		}
	}
	// Wait out the window between a plugin's sync and it being listed.
	rig.runtime.BlockPluginSync().Unblock()
	return rig
}

func expPod(i int) *api.PodSandbox {
	id := fmt.Sprintf("pod-%04d", i)
	return &api.PodSandbox{Id: id, Name: id, Namespace: "exp"}
}

func expContainer(i int) *api.Container {
	id := fmt.Sprintf("ctr-%04d", i)
	return &api.Container{Id: id, Name: id, PodSandboxId: expPod(i).Id, State: api.ContainerState_CONTAINER_CREATED}
}

func uniform(specCount int, events []string, d time.Duration) []expPluginSpec {
	specs := make([]expPluginSpec, specCount)
	for i := range specs {
		specs[i] = expPluginSpec{events: events, delay: map[string]time.Duration{"": d}}
	}
	return specs
}

// burst runs n goroutines issuing op(i) at once and returns the wall time.
func burst(t *testing.T, n int, op func(i int) error) time.Duration {
	t.Helper()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := op(i); err != nil {
				t.Errorf("request %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	return time.Since(start)
}

func expMode() string {
	if m := os.Getenv("NRI_EXP_ORDER"); m != "" {
		return m
	}
	return "baseline"
}

// TestExpCreateMatrix: N concurrent CreateContainer requests through P
// plugins that each sleep d. Prints wall time against the two models:
// fully serialized N*P*d, and pipelined (N+P-1)*d.
func TestExpCreateMatrix(t *testing.T) {
	const d = 10 * time.Millisecond
	counts := []int{1, 8, 32, 128}
	plugins := []int{1, 2, 4}

	t.Logf("mode=%s, plugin delay %v per CreateContainer", expMode(), d)
	t.Logf("%4s %4s %10s %10s %10s %8s", "N", "P", "total", "N*P*d", "(N+P-1)*d", "overlap")
	for _, p := range plugins {
		for _, n := range counts {
			rig := startExpRig(t, uniform(p, []string{"CreateContainer"}, d))
			total := burst(t, n, func(i int) error {
				_, err := rig.runtime.CreateContainer(context.Background(),
					&api.CreateContainerRequest{Pod: expPod(i), Container: expContainer(i)})
				return err
			})
			overlap := 0
			for _, pl := range rig.plugins {
				overlap += pl.Overlap()
			}
			t.Logf("%4d %4d %10v %10v %10v %8d", n, p, total.Round(time.Millisecond),
				time.Duration(n*p)*d, time.Duration(n+p-1)*d, overlap)
		}
	}
}

// TestExpOverhead: the fixed cost of NRI itself (no plugin delay).
func TestExpOverhead(t *testing.T) {
	counts := []int{1, 32, 128}
	plugins := []int{1, 4}

	t.Logf("mode=%s, plugin delay 0", expMode())
	t.Logf("%4s %4s %10s %12s", "N", "P", "total", "per req*plugin")
	for _, p := range plugins {
		for _, n := range counts {
			rig := startExpRig(t, uniform(p, []string{"CreateContainer"}, 0))
			total := burst(t, n, func(i int) error {
				_, err := rig.runtime.CreateContainer(context.Background(),
					&api.CreateContainerRequest{Pod: expPod(i), Container: expContainer(i)})
				return err
			})
			t.Logf("%4d %4d %10v %12v", n, p, total.Round(100*time.Microsecond),
				(total / time.Duration(n*p)).Round(time.Microsecond))
		}
	}
}

// TestExpLifecycleBurst: N pods, each going through the full create-side
// lifecycle (RunPodSandbox, CreateContainer, PostCreateContainer,
// StartContainer, PostStartContainer) and then the stop side
// (StopContainer, RemoveContainer, StopPodSandbox, RemovePodSandbox),
// concurrently. Plugin 0 is "policy-like": slow on CreateContainer and
// StopContainer, instant otherwise. Plugin 1 is "adjuster-like": cheap on
// everything. This is the mixed burst where notification events pay the
// price of the global lock today.
func TestExpLifecycleBurst(t *testing.T) {
	const (
		slow  = 10 * time.Millisecond
		cheap = 1 * time.Millisecond
	)
	counts := []int{8, 32, 64}

	specs := []expPluginSpec{
		{events: allExpEvents, delay: map[string]time.Duration{"": 0, "CreateContainer": slow, "StopContainer": slow}},
		{events: allExpEvents, delay: map[string]time.Duration{"": cheap}},
	}

	t.Logf("mode=%s: plugin0 %v on Create/Stop else 0; plugin1 %v on all 9 events", expMode(), slow, cheap)
	t.Logf("%4s %10s %10s %12s", "N", "up", "down", "serial-model")
	for _, n := range counts {
		rig := startExpRig(t, specs)
		ctx := context.Background()
		up := burst(t, n, func(i int) error {
			pod, ctr := expPod(i), expContainer(i)
			if err := rig.runtime.RunPodSandbox(ctx, &api.RunPodSandboxRequest{Pod: pod}); err != nil {
				return err
			}
			if _, err := rig.runtime.CreateContainer(ctx, &api.CreateContainerRequest{Pod: pod, Container: ctr}); err != nil {
				return err
			}
			if err := rig.runtime.PostCreateContainer(ctx, &api.PostCreateContainerRequest{Pod: pod, Container: ctr}); err != nil {
				return err
			}
			if err := rig.runtime.StartContainer(ctx, &api.StartContainerRequest{Pod: pod, Container: ctr}); err != nil {
				return err
			}
			return rig.runtime.PostStartContainer(ctx, &api.PostStartContainerRequest{Pod: pod, Container: ctr})
		})
		down := burst(t, n, func(i int) error {
			pod, ctr := expPod(i), expContainer(i)
			if _, err := rig.runtime.StopContainer(ctx, &api.StopContainerRequest{Pod: pod, Container: ctr}); err != nil {
				return err
			}
			if err := rig.runtime.RemoveContainer(ctx, &api.RemoveContainerRequest{Pod: pod, Container: ctr}); err != nil {
				return err
			}
			if err := rig.runtime.StopPodSandbox(ctx, &api.StopPodSandboxRequest{Pod: pod}); err != nil {
				return err
			}
			return rig.runtime.RemovePodSandbox(ctx, &api.RemovePodSandboxRequest{Pod: pod})
		})
		// serial model: every event of every pod through both plugins, one at a time
		serialUp := time.Duration(n) * (slow + 5*cheap)
		serialDown := time.Duration(n) * (slow + 4*cheap)
		t.Logf("%4d %10v %10v %12v", n, up.Round(time.Millisecond), down.Round(time.Millisecond),
			fmt.Sprintf("%v/%v", serialUp, serialDown))
	}
}

// inversions counts pairs (i, j) such that plugin saw i before j but the
// runtime got j's answer back before i's. Only pairs both present in both
// sequences are counted.
func inversions(seen, applied []string) int {
	pos := map[string]int{}
	for i, s := range applied {
		pos[s] = i
	}
	var idx []int
	for _, s := range seen {
		if p, ok := pos[s]; ok {
			idx = append(idx, p)
		}
	}
	n := 0
	for a := 0; a < len(idx); a++ {
		for b := a + 1; b < len(idx); b++ {
			if idx[a] > idx[b] {
				n++
			}
		}
	}
	return n
}

// TestExpOrderInversions: a mixed burst of creates and stops through two
// plugins where plugin 0 subscribes to CreateContainer only and is slow,
// and plugin 1 subscribes to both and is fast. Stops skip plugin 0, so a
// design that orders per plugin by lock acquisition (rather than by one
// global sequence) lets plugin 1 see a stop before a create whose answer
// nevertheless comes back first. Counts those inversions.
func TestExpOrderInversions(t *testing.T) {
	const n = 64
	specs := []expPluginSpec{
		{events: []string{"CreateContainer"}, delay: map[string]time.Duration{"": 5 * time.Millisecond}},
		{events: []string{"CreateContainer", "StopContainer"}, delay: map[string]time.Duration{"": 200 * time.Microsecond}},
	}
	rig := startExpRig(t, specs)
	rnd := rand.New(rand.NewSource(1))
	kinds := make([]bool, n)
	for i := range kinds {
		kinds[i] = rnd.Intn(2) == 0
	}
	ctx := context.Background()
	burst(t, n, func(i int) error {
		if kinds[i] {
			_, err := rig.runtime.CreateContainer(ctx, &api.CreateContainerRequest{Pod: expPod(i), Container: expContainer(i)})
			rig.record("CreateContainer", expContainer(i).Id)
			return err
		}
		_, err := rig.runtime.StopContainer(ctx, &api.StopContainerRequest{Pod: expPod(i), Container: expContainer(i)})
		rig.record("StopContainer", expContainer(i).Id)
		return err
	})
	applied := rig.Applied()
	for _, p := range rig.plugins {
		seen := p.Seen()
		inv := inversions(seen, applied)
		t.Logf("mode=%s plugin %s: saw %d requests, %d order inversions against answer order, overlap %d",
			expMode(), p.name, len(seen), inv, p.Overlap())
		if os.Getenv("NRI_EXP_EXPECT_INVERSIONS") == "" {
			require.Zero(t, inv, "plugin %s: answers applied in a different order than the plugin decided them", p.name)
		}
	}
}

// TestExpSameContainerOrder: with concurrent requests, a plugin must still
// see the lifecycle of one container in order. The runtime issues them in
// order per container (it waits for each to return), so this holds in any
// design; the test pins it.
func TestExpSameContainerOrder(t *testing.T) {
	const n = 32
	rig := startExpRig(t, uniform(2, allExpEvents, 500*time.Microsecond))
	ctx := context.Background()
	burst(t, n, func(i int) error {
		pod, ctr := expPod(i), expContainer(i)
		if _, err := rig.runtime.CreateContainer(ctx, &api.CreateContainerRequest{Pod: pod, Container: ctr}); err != nil {
			return err
		}
		if err := rig.runtime.StartContainer(ctx, &api.StartContainerRequest{Pod: pod, Container: ctr}); err != nil {
			return err
		}
		if _, err := rig.runtime.StopContainer(ctx, &api.StopContainerRequest{Pod: pod, Container: ctr}); err != nil {
			return err
		}
		return rig.runtime.RemoveContainer(ctx, &api.RemoveContainerRequest{Pod: pod, Container: ctr})
	})
	want := []string{"CreateContainer", "StartContainer", "StopContainer", "RemoveContainer"}
	for _, p := range rig.plugins {
		perCtr := map[string][]string{}
		for _, s := range p.Seen() {
			ev, id, _ := strings.Cut(s, ":")
			perCtr[id] = append(perCtr[id], ev)
		}
		ids := make([]string, 0, len(perCtr))
		for id := range perCtr {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			require.Equal(t, want, perCtr[id], "plugin %s container %s", p.name, id)
		}
	}
	t.Logf("mode=%s: per-container lifecycle order held for %d containers on %d plugins", expMode(), n, len(rig.plugins))
}

// TestExpBurstDoesNotExpireQueuedRequests: a burst that takes longer than
// the plugin request timeout must not fail requests that were merely
// queued. The timeout must bound the plugin's handling, not the wait.
func TestExpBurstDoesNotExpireQueuedRequests(t *testing.T) {
	const (
		n       = 50
		d       = 10 * time.Millisecond
		timeout = 200 * time.Millisecond
	)
	old := nri.DefaultPluginRequestTimeout
	nri.SetPluginRequestTimeout(timeout)
	t.Cleanup(func() { nri.SetPluginRequestTimeout(old) })

	rig := startExpRig(t, uniform(1, []string{"CreateContainer"}, d))
	var failed int64
	var mu sync.Mutex
	total := burst(t, n, func(i int) error {
		_, err := rig.runtime.CreateContainer(context.Background(),
			&api.CreateContainerRequest{Pod: expPod(i), Container: expContainer(i)})
		if err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
		}
		return nil
	})
	t.Logf("mode=%s: %d creates x %v through 1 plugin with a %v request timeout took %v, %d failed",
		expMode(), n, d, timeout, total.Round(time.Millisecond), failed)
	require.Zero(t, failed)
}

// TestExpCancelledWaiter: a request whose context is cancelled while it is
// queued behind another request should come back promptly (prototype) and
// must not block the requests queued behind it.
func TestExpCancelledWaiter(t *testing.T) {
	const d = 100 * time.Millisecond
	rig := startExpRig(t, uniform(1, []string{"CreateContainer"}, d))
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = rig.runtime.CreateContainer(ctx, &api.CreateContainerRequest{Pod: expPod(0), Container: expContainer(0)})
	}()
	time.Sleep(10 * time.Millisecond) // A is inside the plugin

	bctx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, errB := rig.runtime.CreateContainer(bctx, &api.CreateContainerRequest{Pod: expPod(1), Container: expContainer(1)})
	tookB := time.Since(start)

	start = time.Now()
	_, errC := rig.runtime.CreateContainer(ctx, &api.CreateContainerRequest{Pod: expPod(2), Container: expContainer(2)})
	tookC := time.Since(start)
	wg.Wait()

	t.Logf("mode=%s: B (5ms deadline, queued behind a %v request) returned after %v with err=%v; C after %v with err=%v",
		expMode(), d, tookB.Round(time.Millisecond), errB, tookC.Round(time.Millisecond), errC)
	require.NoError(t, errC)
}
