package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

const (
	maxCodeBytes   = 128 << 10 // 128 KiB of source is plenty
	maxTimeout     = 60 * time.Second
	defaultTimeout = 10 * time.Second
	maxMemoryBytes = 1 << 30 // callers can't ask for more than 1 GiB

	// Admission control. Per-container limits bound what ONE execution can
	// consume; these bound how many can exist at once, so N concurrent
	// requests can't multiply 256 MiB x 0.5 CPU into the whole host.
	DefaultMaxConcurrent = 4  // containers running simultaneously
	DefaultMaxQueued     = 16 // additionally admitted and waiting for a slot
)

// Limits is the service-wide capacity policy.
type Capacity struct {
	MaxConcurrent int // containers allowed to run at once
	MaxQueued     int // executions allowed to wait for a slot
}

func DefaultCapacity() Capacity {
	return Capacity{MaxConcurrent: DefaultMaxConcurrent, MaxQueued: DefaultMaxQueued}
}

type Server struct {
	exec     executor.Executor
	registry *Registry
	stats    *Stats
	token    string
	upgrader websocket.Upgrader

	// slots is a counting semaphore: one token per concurrently running
	// container. inFlight counts running + queued, so admission can reject
	// before the queue grows without bound.
	slots       chan struct{}
	inFlight    atomic.Int64
	maxInFlight int64

	// sessions is nil unless the executor supports long-lived sandboxes.
	sessions *SessionRegistry
}

func NewServer(exec executor.Executor, token string) *Server {
	return NewServerWithCapacity(exec, token, DefaultCapacity())
}

func NewServerWithCapacity(exec executor.Executor, token string, cap Capacity) *Server {
	if cap.MaxConcurrent < 1 {
		cap.MaxConcurrent = 1
	}
	if cap.MaxQueued < 0 {
		cap.MaxQueued = 0
	}
	s := &Server{
		exec:     exec,
		registry: NewRegistry(),
		stats:    &Stats{},
		token:    token,
		upgrader: websocket.Upgrader{
			// Demo service: allow the dev frontend origin. Tighten before deploy.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		slots:       make(chan struct{}, cap.MaxConcurrent),
		maxInFlight: int64(cap.MaxConcurrent + cap.MaxQueued),
	}
	// Enable sessions only if the backend supports them. Cap live sessions at
	// the same concurrency as runs; reap after 10 minutes idle.
	if se, ok := exec.(executor.SessionExecutor); ok {
		s.sessions = NewSessionRegistry(se, cap.MaxConcurrent, 10*time.Minute)
	}
	return s
}

// Shutdown destroys any live sessions so the process leaves no orphaned
// containers behind.
func (s *Server) Shutdown() {
	if s.sessions != nil {
		s.sessions.Shutdown()
	}
}

// admit reserves capacity for one execution, or reports that the service is
// full. Callers that admit successfully MUST call release exactly once.
func (s *Server) admit() bool {
	for {
		cur := s.inFlight.Load()
		if cur >= s.maxInFlight {
			return false
		}
		if s.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (s *Server) release() { s.inFlight.Add(-1) }

func (s *Server) Handler() http.Handler {
	return s.HandlerWithUI(nil)
}

// HandlerWithUI mounts the API and, when assets is non-nil, serves the
// embedded frontend from "/". API routes are registered as explicit patterns
// so they always win over the catch-all static route.
func (s *Server) HandlerWithUI(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /execute", s.auth(s.handleExecute))
	mux.HandleFunc("POST /execute/sync", s.auth(s.handleExecuteSync))
	mux.HandleFunc("GET /stream/{id}", s.auth(s.handleStream))
	mux.HandleFunc("GET /stats", s.auth(s.handleStats))
	mux.HandleFunc("GET /config", s.auth(s.handleConfig))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if s.sessions != nil {
		mux.HandleFunc("POST /sessions", s.auth(s.handleSessionCreate))
		mux.HandleFunc("GET /sessions", s.auth(s.handleSessionList))
		mux.HandleFunc("POST /sessions/{id}/exec", s.auth(s.handleSessionExec))
		mux.HandleFunc("DELETE /sessions/{id}", s.auth(s.handleSessionDestroy))
	}
	if assets != nil {
		mux.Handle("GET /", StaticHandler(assets))
	}
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		// Browsers can't set headers on WebSocket dials; accept ?token= there too.
		if got == "" {
			got = "Bearer " + r.URL.Query().Get("token")
		}
		want := "Bearer " + s.token
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type executeRequest struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Limits   struct {
		MemoryBytes int64   `json:"memoryBytes"`
		CPUQuota    float64 `json:"cpuQuota"`
		PIDsMax     int64   `json:"pidsMax"`
	} `json:"limits"`
	TimeoutMS int64 `json:"timeoutMs"`
}

func (s *Server) parseRequest(w http.ResponseWriter, r *http.Request) (executor.ExecRequest, bool) {
	var body executeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCodeBytes+4096)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return executor.ExecRequest{}, false
	}
	if body.Code == "" || len(body.Code) > maxCodeBytes {
		http.Error(w, "code must be 1..131072 bytes", http.StatusBadRequest)
		return executor.ExecRequest{}, false
	}
	if body.Language == "" {
		body.Language = "python"
	}

	limits := executor.DefaultLimits()
	if body.Limits.MemoryBytes > 0 {
		limits.MemoryBytes = min(body.Limits.MemoryBytes, maxMemoryBytes)
	}
	if body.Limits.CPUQuota > 0 {
		limits.CPUQuota = min(body.Limits.CPUQuota, 2.0)
	}
	if body.Limits.PIDsMax > 0 {
		limits.PIDsMax = min(body.Limits.PIDsMax, 256)
	}
	timeout := defaultTimeout
	if body.TimeoutMS > 0 {
		timeout = min(time.Duration(body.TimeoutMS)*time.Millisecond, maxTimeout)
	}
	return executor.ExecRequest{
		Language: body.Language,
		Code:     body.Code,
		Limits:   limits,
		Timeout:  timeout,
	}, true
}

// run drives one execution to completion, publishing into the registry record.
// The caller must have admitted this execution; run releases it.
func (s *Server) run(e *Execution, req executor.ExecRequest) {
	defer s.release()

	// Wait for a concurrency slot. Queue wait is measured separately from
	// execution time so a busy service is distinguishable from a slow one.
	queuedAt := time.Now()
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	e.setQueueWait(time.Since(queuedAt))

	out := make(chan executor.OutputChunk, 256)
	published := make(chan struct{})
	go func() {
		defer close(published)
		for c := range out {
			e.publish(c)
		}
	}()
	// Background context: the execution's lifetime is its own timeout,
	// not the HTTP request that launched it.
	res, err := s.exec.Execute(context.Background(), req, out)
	// Execute closed out, so this drains and returns. Wait for it before
	// finishing: subscribers treat "done" as "no more output is coming".
	<-published
	if err == nil {
		s.stats.Record(res.StartupMS, res.DurationMS)
	} else {
		log.Printf("execution %s failed: %v", e.ID, err)
	}
	e.finish(res, err)
}

// tooBusy rejects when the service is at capacity. 429 + Retry-After is the
// honest answer: better to refuse fast than to queue without bound and let
// every request time out.
func (s *Server) tooBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "server at capacity: too many executions in flight", http.StatusTooManyRequests)
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseRequest(w, r)
	if !ok {
		return
	}
	if !s.admit() {
		s.tooBusy(w)
		return
	}
	e := s.registry.New()
	go s.run(e, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"executionId": e.ID})
}

func (s *Server) handleExecuteSync(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseRequest(w, r)
	if !ok {
		return
	}
	if !s.admit() {
		s.tooBusy(w)
		return
	}
	e := s.registry.New()
	go s.run(e, req)
	_, live, done, cancel := e.Subscribe()
	defer cancel()

	var stdout, stderr []byte
	for {
		select {
		case c := <-live:
			if c.Stream == "stderr" {
				stderr = append(stderr, c.Data...)
			} else {
				stdout = append(stdout, c.Data...)
			}
		case <-done:
			// Drain anything still buffered in the subscription channel.
			for {
				select {
				case c := <-live:
					if c.Stream == "stderr" {
						stderr = append(stderr, c.Data...)
					} else {
						stdout = append(stdout, c.Data...)
					}
					continue
				default:
				}
				break
			}
			res, err := e.Result()
			if err != nil {
				http.Error(w, "execution failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"executionId": e.ID,
				"stdout":      string(stdout),
				"stderr":      string(stderr),
				"result":      res,
				"queueWaitMs": e.QueueWaitMS(),
			})
			return
		}
	}
}

// Wire frames for /stream: {"type":"chunk",...} then a final {"type":"result",...}.
type streamFrame struct {
	Type        string               `json:"type"` // "chunk" | "result" | "error"
	Stream      string               `json:"stream,omitempty"`
	Data        string               `json:"data,omitempty"`
	TS          time.Time            `json:"ts,omitempty"`
	Result      *executor.ExecResult `json:"result,omitempty"`
	QueueWaitMS int64                `json:"queueWaitMs,omitempty"`
	Error       string               `json:"error,omitempty"`
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	e, ok := s.registry.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown execution", http.StatusNotFound)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	replay, live, done, cancel := e.Subscribe()
	defer cancel()

	send := func(f streamFrame) bool {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(f) == nil
	}
	chunkFrame := func(c executor.OutputChunk) streamFrame {
		return streamFrame{Type: "chunk", Stream: c.Stream, Data: string(c.Data), TS: c.TS}
	}

	for _, c := range replay {
		if !send(chunkFrame(c)) {
			return
		}
	}
	for {
		select {
		case c := <-live:
			if !send(chunkFrame(c)) {
				return
			}
		case <-done:
			for {
				select {
				case c := <-live:
					if !send(chunkFrame(c)) {
						return
					}
					continue
				default:
				}
				break
			}
			res, err := e.Result()
			if err != nil {
				send(streamFrame{Type: "error", Error: err.Error()})
			} else {
				send(streamFrame{Type: "result", Result: res, QueueWaitMS: e.QueueWaitMS()})
			}
			return
		}
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	sum := s.stats.Summary()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":        sum.Count,
		"startupP50Ms": sum.StartupP50,
		"startupP99Ms": sum.StartupP99,
		"totalP50Ms":   sum.TotalP50,
		"totalP99Ms":   sum.TotalP99,
		// Live capacity, so the UI (and you) can see saturation.
		"inFlight":      s.inFlight.Load(),
		"running":       len(s.slots),
		"maxConcurrent": cap(s.slots),
		"maxInFlight":   s.maxInFlight,
	})
}

// handleConfig reports the isolation controls the active backend actually
// applies, so the UI displays live configuration rather than a hardcoded
// list that could drift from reality.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	d, ok := s.exec.(executor.Describer)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"backend": "unknown"})
		return
	}
	desc := d.Describe()
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":       desc.Backend,
		"image":         desc.Image,
		"runtime":       desc.Runtime,
		"controls":      desc.Controls,
		"maxConcurrent": cap(s.slots),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
