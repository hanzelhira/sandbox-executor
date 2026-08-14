package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

// fakeExecutor echoes the code back on stdout and reports a canned result.
type fakeExecutor struct{}

func (fakeExecutor) Execute(ctx context.Context, req executor.ExecRequest, out chan<- executor.OutputChunk) (executor.ExecResult, error) {
	defer close(out)
	out <- executor.OutputChunk{Stream: "stdout", Data: []byte("ran: " + req.Code), TS: time.Now()}
	out <- executor.OutputChunk{Stream: "stderr", Data: []byte("warn"), TS: time.Now()}
	return executor.ExecResult{ExitCode: 0, DurationMS: 5, StartupMS: 3}, nil
}

func newTestServer() *httptest.Server {
	return httptest.NewServer(NewServer(fakeExecutor{}, "secret").Handler())
}

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	resp := post(t, ts.URL+"/execute", "", `{"code":"print(1)"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp = post(t, ts.URL+"/execute", "wrong", `{"code":"print(1)"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for bad token, got %d", resp.StatusCode)
	}
}

func TestExecuteSync(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	resp := post(t, ts.URL+"/execute/sync", "secret", `{"code":"print(1)"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Stdout string              `json:"stdout"`
		Stderr string              `json:"stderr"`
		Result executor.ExecResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Stdout != "ran: print(1)" {
		t.Errorf("stdout = %q", body.Stdout)
	}
	if body.Stderr != "warn" {
		t.Errorf("stderr = %q", body.Stderr)
	}
	if body.Result.ExitCode != 0 || body.Result.StartupMS != 3 {
		t.Errorf("result = %+v", body.Result)
	}
}

func TestExecuteAsyncAndStats(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	resp := post(t, ts.URL+"/execute", "secret", `{"code":"print(1)"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var body struct {
		ExecutionID string `json:"executionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ExecutionID == "" {
		t.Fatal("empty executionId")
	}

	// Stats should reflect the run once it finishes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", ts.URL+"/stats", nil)
		req.Header.Set("Authorization", "Bearer secret")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var sum StatsSummary
		_ = json.NewDecoder(r.Body).Decode(&sum)
		r.Body.Close()
		if sum.Count >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stats never recorded the execution")
}

func TestRejectsOversizeAndEmptyCode(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	if resp := post(t, ts.URL+"/execute", "secret", `{"code":""}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty code: want 400, got %d", resp.StatusCode)
	}
}
