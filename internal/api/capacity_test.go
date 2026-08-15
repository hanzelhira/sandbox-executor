package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

// blockingExecutor holds each execution until released, and records the
// high-water mark of concurrent executions so tests can assert the cap.
type blockingExecutor struct {
	release chan struct{}

	mu      sync.Mutex
	current int
	peak    int
	started atomic.Int64
}

func (b *blockingExecutor) Execute(ctx context.Context, req executor.ExecRequest, out chan<- executor.OutputChunk) (executor.ExecResult, error) {
	defer close(out)
	b.started.Add(1)
	b.mu.Lock()
	b.current++
	if b.current > b.peak {
		b.peak = b.current
	}
	b.mu.Unlock()

	<-b.release // hold the slot until the test says go

	b.mu.Lock()
	b.current--
	b.mu.Unlock()
	return executor.ExecResult{ExitCode: 0, StartupMS: 1, DurationMS: 1}, nil
}

func (b *blockingExecutor) peakConcurrent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

func postCode(url, token string) *http.Response {
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{"code":"print(1)"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// Never more than MaxConcurrent containers exist at once, no matter how many
// requests arrive — this is the property that stops N users from multiplying
// per-container limits into the whole host.
func TestConcurrencyCapIsEnforced(t *testing.T) {
	blocker := &blockingExecutor{release: make(chan struct{})}
	srv := NewServerWithCapacity(blocker, "secret", Capacity{MaxConcurrent: 3, MaxQueued: 10})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := 0; i < 13; i++ {
		resp := postCode(ts.URL+"/execute", "secret")
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("request %d: want 202, got %d", i, resp.StatusCode)
		}
	}

	// Give the runners time to grab whatever slots they can.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && blocker.started.Load() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let any over-admission surface

	if peak := blocker.peakConcurrent(); peak > 3 {
		t.Errorf("peak concurrent executions = %d, want <= 3", peak)
	}
	if started := blocker.started.Load(); started > 3 {
		t.Errorf("%d executions started, want <= 3 running at once", started)
	}
	close(blocker.release)
}

// Past concurrent+queued, the service refuses fast with 429 instead of
// queueing without bound.
func TestOverCapacityReturns429(t *testing.T) {
	blocker := &blockingExecutor{release: make(chan struct{})}
	srv := NewServerWithCapacity(blocker, "secret", Capacity{MaxConcurrent: 2, MaxQueued: 2})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 4 fill capacity (2 running + 2 queued).
	for i := 0; i < 4; i++ {
		resp := postCode(ts.URL+"/execute", "secret")
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("request %d: want 202, got %d", i, resp.StatusCode)
		}
	}
	// The 5th must be refused.
	resp := postCode(ts.URL+"/execute", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429 when full, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 should carry Retry-After")
	}

	close(blocker.release)

	// After draining, the service accepts work again.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := postCode(ts.URL+"/execute", "secret")
		r.Body.Close()
		if r.StatusCode == http.StatusAccepted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("service never recovered capacity after executions drained")
}

// Capacity must be released on every path, or the service leaks slots and
// eventually wedges at 429 forever.
func TestCapacityIsReleasedAfterRuns(t *testing.T) {
	blocker := &blockingExecutor{release: make(chan struct{})}
	close(blocker.release) // don't block: every run completes immediately
	srv := NewServerWithCapacity(blocker, "secret", Capacity{MaxConcurrent: 2, MaxQueued: 2})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := 0; i < 20; i++ {
		resp := postCode(ts.URL+"/execute/sync", "secret")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("run %d: want 200, got %d", i, resp.StatusCode)
		}
	}

	req, _ := http.NewRequest("GET", ts.URL+"/stats", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if got := stats["inFlight"].(float64); got != 0 {
		t.Errorf("inFlight = %v after all runs finished, want 0 (slot leak)", got)
	}
}
