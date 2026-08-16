package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	// Reuse the run-request parser for limits, ignoring the code field.
	var body executeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && err.Error() != "EOF" {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	spec := executor.SessionSpec{
		Language: "python",
		Limits:   clampLimits(body.Limits.MemoryBytes, body.Limits.CPUQuota, body.Limits.PIDsMax),
	}
	rec, err := s.sessions.Create(r.Context(), spec)
	if errors.Is(err, ErrAtCapacity) {
		s.tooBusy(w)
		return
	}
	if err != nil {
		http.Error(w, "create session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sessionId": rec.ID})
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.sessions.List(),
		"max":      s.sessions.maxLive,
	})
}

func (s *Server) handleSessionDestroy(w http.ResponseWriter, r *http.Request) {
	err := s.sessions.Destroy(r.PathValue("id"))
	if errors.Is(err, ErrNoSuchSession) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "destroy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionExec runs code in an existing session. Like /execute it returns
// an executionId immediately and streams over the shared /stream/{id} socket.
func (s *Server) handleSessionExec(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	req, ok := s.parseRequest(w, r)
	if !ok {
		return
	}
	// Admission still applies: a session exec consumes a concurrency slot just
	// like a one-shot run.
	if !s.admit() {
		s.tooBusy(w)
		return
	}
	e := s.registry.New()
	go s.runInSession(e, rec, req.Code, req.Timeout)
	writeJSON(w, http.StatusAccepted, map[string]string{"executionId": e.ID})
}

// runInSession mirrors run(), but execs into a live session instead of creating
// a throwaway container.
func (s *Server) runInSession(e *Execution, rec *SessionRecord, code string, timeout time.Duration) {
	defer s.release()

	queuedAt := time.Now()
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	e.setQueueWait(time.Since(queuedAt))

	rec.setBusy(true)
	defer rec.setBusy(false)
	rec.touch()

	out := make(chan executor.OutputChunk, 256)
	published := make(chan struct{})
	go func() {
		defer close(published)
		for c := range out {
			e.publish(c)
		}
	}()
	res, err := rec.sess.Exec(context.Background(), code, timeout, out)
	<-published
	if err == nil {
		s.stats.Record(res.StartupMS, res.DurationMS)
	} else {
		log.Printf("session exec %s failed: %v", e.ID, err)
	}
	e.finish(res, err)
}

// clampLimits mirrors parseRequest's clamping for the session-create path.
func clampLimits(memBytes int64, cpu float64, pids int64) executor.ResourceLimits {
	limits := executor.DefaultLimits()
	if memBytes > 0 {
		limits.MemoryBytes = min(memBytes, maxMemoryBytes)
	}
	if cpu > 0 {
		limits.CPUQuota = min(cpu, 2.0)
	}
	if pids > 0 {
		limits.PIDsMax = min(pids, 256)
	}
	return limits
}
