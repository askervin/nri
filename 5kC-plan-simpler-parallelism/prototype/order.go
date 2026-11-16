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

package adaptation

import (
	"context"
	"os"
	"strings"
	"sync"
)

// The order requests are relayed to plugins and applied in.
//
// PROTOTYPE for the 5kC parallelism research. Three modes, selected with
// NRI_EXP_ORDER so that one binary can be measured in each:
//
//   global (default): one sequence number per request, taken as it enters.
//     Every plugin the request will reach gets a ticket chained behind the
//     previous ticket handed out for that plugin, and a request that can
//     answer with container changes gets an apply ticket chained behind the
//     previous apply ticket. A plugin therefore sees requests one at a time
//     in entry order, and answers are applied in entry order. This is the
//     ordering the global lock gives today, without holding anything while
//     a plugin thinks.
//
//   mutex: a plugin sees one request at a time, but in the order the
//     requests happen to reach it (a per-plugin mutex). Answers still apply
//     in entry order. Shown to be unsound by TestExpOrderInversions.
//
//   none: no per-plugin serialization. Equivalent to every plugin having
//     declared parallel event handling.
//
// Tickets are chained channels, so there is no map of waiters, no hole
// bookkeeping and nothing to arm: a ticket exists only for a plugin the
// request will actually reach, and a request that stops early releases
// the tickets it did not use. Nothing blocks while holding a ticket except
// the plugin call it serializes, and every wait is on a lower sequence
// number, so there is no cycle to deadlock on.

type orderMode int

const (
	orderGlobal orderMode = iota
	orderMutex
	orderNone
)

func expOrderMode() orderMode {
	switch strings.ToLower(os.Getenv("NRI_EXP_ORDER")) {
	case "mutex":
		return orderMutex
	case "none":
		return orderNone
	}
	return orderGlobal
}

// expParallel reports whether the named plugin stands in for one that
// declared parallel event handling (NRI_EXP_PARALLEL_PLUGINS=all|name,...).
func expParallel(name string) bool {
	v := os.Getenv("NRI_EXP_PARALLEL_PLUGINS")
	if v == "" {
		return false
	}
	if v == "all" {
		return true
	}
	for _, n := range strings.Split(v, ",") {
		n = strings.TrimSpace(n)
		if n == name || strings.HasSuffix(name, "-"+n) {
			return true
		}
	}
	return false
}

// order hands out turns.
type order struct {
	mu    sync.Mutex
	mode  orderMode
	seq   uint64
	tails map[*plugin]chan struct{} // done channel of the last ticket per plugin
	apply chan struct{}             // done channel of the last apply ticket
}

// ticket is one place in one chain.
type ticket struct {
	prev <-chan struct{} // closed when the predecessor is done; nil if none
	done chan struct{}
}

func (tk *ticket) wait(ctx context.Context) error {
	if tk == nil || tk.prev == nil {
		return nil
	}
	select {
	case <-tk.prev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// turn is one request's place in the order: a ticket per plugin it will
// reach and, if it can answer with changes, an apply ticket.
type turn struct {
	seq     uint64
	mode    orderMode
	tickets map[*plugin]*ticket
	locked  map[*plugin]bool // mutex mode
	apply   *ticket
}

// enter gives a request its place. plugins is the live, sorted plugin list
// and must be stable while the turn is held (the caller holds the
// Adaptation read lock).
func (o *order) enter(plugins []*plugin, event Event, applies bool) *turn {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.tails == nil {
		o.tails = map[*plugin]chan struct{}{}
		o.mode = expOrderMode()
	}

	o.seq++
	t := &turn{seq: o.seq, mode: o.mode, tickets: map[*plugin]*ticket{}, locked: map[*plugin]bool{}}

	if o.mode == orderGlobal {
		for _, p := range plugins {
			if !p.events.IsSet(event) || p.parallel || p.isClosed() {
				continue
			}
			tk := &ticket{prev: o.tails[p], done: make(chan struct{})}
			o.tails[p] = tk.done
			t.tickets[p] = tk
		}
	}

	if applies {
		t.apply = &ticket{prev: o.apply, done: make(chan struct{})}
		o.apply = t.apply.done
	}

	return t
}

// forget drops the chain tail of a plugin that is gone.
func (o *order) forget(p *plugin) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.tails, p)
}

// wait blocks until it is this request's turn at p.
func (t *turn) wait(ctx context.Context, p *plugin) error {
	switch t.mode {
	case orderMutex:
		if p.parallel {
			return nil
		}
		p.serial.Lock()
		t.locked[p] = true
		return nil
	case orderNone:
		return nil
	}
	return t.tickets[p].wait(ctx)
}

// done hands the turn at p to the next request.
func (t *turn) done(p *plugin) {
	if t.locked[p] {
		delete(t.locked, p)
		p.serial.Unlock()
	}
	if tk := t.tickets[p]; tk != nil {
		delete(t.tickets, p)
		close(tk.done)
	}
}

// waitApply blocks until every earlier request that applies has applied.
func (t *turn) waitApply(ctx context.Context) error {
	return t.apply.wait(ctx)
}

// release hands on every turn this request still holds. Deferred by every
// request, so a request that fails or bails out early never leaves a chain
// waiting.
func (t *turn) release() {
	for p := range t.locked {
		t.done(p)
	}
	for p := range t.tickets {
		t.done(p)
	}
	if t.apply != nil {
		close(t.apply.done)
		t.apply = nil
	}
}
