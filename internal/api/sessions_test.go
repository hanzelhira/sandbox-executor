package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

// fakeSessionExec is a fake that supports sessions, for testing the registry
// and handlers without Docker.
type fakeSessionExec struct {
	fakeExecutor
	mu        sync.Mutex
	created   int
	destroyed int
}

func (f *fakeSessionExec) CreateSession(ctx context.Context, spec executor.SessionSpec) (executor.Session, error) {
	f.mu.Lock()
	f.created++
	f.mu.Unlock()
	return &fakeSession{parent: f, state: map[string]string{}}, nil
}

type fakeSession struct {
	parent *fakeSessionExec
	mu     sync.Mutex
	state  map[string]string
	dead   bool
}

func (s *fakeSession) ID() string { return "fake" }

func (s *fakeSession) Exec(ctx context.Context, code string, timeout time.Duration, out chan<- executor.OutputChunk) (executor.ExecResult, error) {
	defer close(out)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		out <- chunk("stderr", "session dead")
		return executor.ExecResult{ExitCode: 1}, nil
	}
	// Toy state machine: "set k=v" stores, "get k" echoes.
	switch {
	case strings.HasPrefix(code, "set "):
		kv := strings.SplitN(strings.TrimPrefix(code, "set "), "=", 2)
		if len(kv) == 2 {
			s.state[kv[0]] = kv[1]
		}
	case strings.HasPrefix(code, "get "):
		out <- chunk("stdout", s.state[strings.TrimPrefix(code, "get ")])
	default:
		out <- chunk("stdout", "ran: "+code)
	}
	return executor.ExecResult{ExitCode: 0, StartupMS: 2, DurationMS: 1}, nil
}

func (s *fakeSession) ExecArgv(ctx context.Context, argv []string, cwd string, timeout time.Duration, out chan<- executor.OutputChunk) (executor.ExecResult, error) {
	return s.Exec(ctx, strings.Join(argv, " "), timeout, out)
}

func (s *fakeSession) Populate(ctx context.Context, files []executor.RepoFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		s.state["file:"+f.Path] = string(f.Data)
	}
	return nil
}

func (s *fakeSession) Healthy(ctx context.Context) bool { return !s.dead }

func (s *fakeSession) Destroy(ctx context.Context) error {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	s.parent.mu.Lock()
	s.parent.destroyed++
	s.parent.mu.Unlock()
	return nil
}

func chunk(stream, data string) executor.OutputChunk {
	return executor.OutputChunk{Stream: stream, Data: []byte(data), TS: time.Now()}
}

func newSessionTestServer(t *testing.T) (*httptest.Server, *fakeSessionExec) {
	t.Helper()
	fake := &fakeSessionExec{}
	srv := NewServerWithCapacity(fake, "secret", Capacity{MaxConcurrent: 2, MaxQueued: 4})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Shutdown(); ts.Close() })
	return ts, fake
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var r *http.Request
	if body != "" {
		r, _ = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		r, _ = http.NewRequest(method, url, nil)
	}
	r.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSessionCreateAndExec(t *testing.T) {
	ts, _ := newSessionTestServer(t)

	resp := do(t, "POST", ts.URL+"/sessions", `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", resp.StatusCode)
	}
	var cr struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if cr.SessionID == "" {
		t.Fatal("empty sessionId")
	}

	// exec should return 202 with an executionId
	resp = do(t, "POST", ts.URL+"/sessions/"+cr.SessionID+"/exec", `{"code":"set x=42"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("exec: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionListAndDestroy(t *testing.T) {
	ts, fake := newSessionTestServer(t)

	resp := do(t, "POST", ts.URL+"/sessions", `{}`)
	var cr struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()

	resp = do(t, "GET", ts.URL+"/sessions", "")
	var list struct {
		Sessions []SessionInfo `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Sessions) != 1 {
		t.Fatalf("want 1 session listed, got %d", len(list.Sessions))
	}

	resp = do(t, "DELETE", ts.URL+"/sessions/"+cr.SessionID, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	fake.mu.Lock()
	destroyed := fake.destroyed
	fake.mu.Unlock()
	if destroyed != 1 {
		t.Errorf("session container not destroyed: destroyed=%d", destroyed)
	}
}

func TestSessionCapEnforced(t *testing.T) {
	ts, _ := newSessionTestServer(t) // MaxConcurrent 2

	for i := 0; i < 2; i++ {
		resp := do(t, "POST", ts.URL+"/sessions", `{}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("session %d: want 201, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := do(t, "POST", ts.URL+"/sessions", `{}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("3rd session past cap: want 429, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionExecOnMissingSession(t *testing.T) {
	ts, _ := newSessionTestServer(t)
	resp := do(t, "POST", ts.URL+"/sessions/nope/exec", `{"code":"print(1)"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for missing session, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
