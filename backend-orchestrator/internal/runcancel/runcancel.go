// Package runcancel lets a Stop reach an executor run in flight in this
// process.
//
// Live runs arrive through a Redis correlation request, not an HTTP request or
// a Temporal activity the orchestrator owns, so nothing outside the run held
// its context: a Stop ended the sink worker and the Temporal workflow, and the
// export kept reading the source (KI-STOP-DOES-NOT-REACH-RUNNING-TRANSFER).
// A run registers its cancel func here for as long as it runs; the Stop
// handler cancels every run of that pipeline.
package runcancel

import (
	"errors"
	"sync"
)

// ErrStopped is the cause a run's context carries when a Stop cancelled it,
// so the run reports "cancelled" rather than a failure or a timeout.
var ErrStopped = errors.New("pipeline stopped by user")

var (
	mu     sync.Mutex
	nextID uint64
	runs   = map[string]map[uint64]func(){}
)

// Register records cancel as a run of pipelineID. The returned func removes
// it and is safe to call more than once; call it when the run returns.
func Register(pipelineID string, cancel func()) (unregister func()) {
	mu.Lock()
	nextID++
	id := nextID
	if runs[pipelineID] == nil {
		runs[pipelineID] = map[uint64]func(){}
	}
	runs[pipelineID][id] = cancel
	mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			delete(runs[pipelineID], id)
			if len(runs[pipelineID]) == 0 {
				delete(runs, pipelineID)
			}
		})
	}
}

// Cancel cancels every registered run of pipelineID and returns how many it
// reached. A run stays registered until it unwinds.
func Cancel(pipelineID string) int {
	if pipelineID == "" {
		return 0
	}
	mu.Lock()
	cancels := make([]func(), 0, len(runs[pipelineID]))
	for _, c := range runs[pipelineID] {
		cancels = append(cancels, c)
	}
	mu.Unlock()
	// Outside the lock: a cancel func may unwind a run that unregisters.
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}
