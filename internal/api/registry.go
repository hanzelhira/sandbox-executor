package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

// Execution is one run's in-memory record: buffered output for late/replayed
// subscribers, live fan-out to attached streams, and the final result.
type Execution struct {
	ID string

	mu     sync.Mutex
	chunks []executor.OutputChunk
	subs   map[chan executor.OutputChunk]struct{}
	result *executor.ExecResult
	err    error
	done   chan struct{}
}

func (e *Execution) publish(c executor.OutputChunk) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chunks = append(e.chunks, c)
	for sub := range e.subs {
		select {
		case sub <- c:
		default: // slow subscriber: drop rather than block the execution
		}
	}
}

func (e *Execution) finish(res executor.ExecResult, err error) {
	e.mu.Lock()
	e.result = &res
	e.err = err
	e.mu.Unlock()
	close(e.done)
}

// Subscribe returns already-buffered chunks plus a channel of future ones,
// and an unsubscribe func. The done channel closes when the run completes.
func (e *Execution) Subscribe() (replay []executor.OutputChunk, live <-chan executor.OutputChunk, done <-chan struct{}, cancel func()) {
	ch := make(chan executor.OutputChunk, 256)
	e.mu.Lock()
	replay = append([]executor.OutputChunk(nil), e.chunks...)
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return replay, ch, e.done, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

func (e *Execution) Result() (*executor.ExecResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result, e.err
}

type Registry struct {
	mu   sync.Mutex
	runs map[string]*Execution
}

func NewRegistry() *Registry {
	return &Registry{runs: make(map[string]*Execution)}
}

func (r *Registry) New() *Execution {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	e := &Execution{
		ID:   hex.EncodeToString(b),
		subs: make(map[chan executor.OutputChunk]struct{}),
		done: make(chan struct{}),
	}
	r.mu.Lock()
	r.runs[e.ID] = e
	r.mu.Unlock()
	return e
}

func (r *Registry) Get(id string) (*Execution, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runs[id]
	return e, ok
}
