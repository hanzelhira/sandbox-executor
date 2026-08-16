package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
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
// sessionExecRequest is the exec body: either code (a Python snippet) or argv
// (an arbitrary command against the repo), with an optional working dir.
type sessionExecRequest struct {
	Code      string   `json:"code"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	TimeoutMS int64    `json:"timeoutMs"`
}

func (s *Server) handleSessionExec(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	var body sessionExecRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCodeBytes+4096)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Code) == 0 && len(body.Argv) == 0 {
		http.Error(w, "provide code or argv", http.StatusBadRequest)
		return
	}
	if len(body.Code) > maxCodeBytes {
		http.Error(w, "code too large", http.StatusBadRequest)
		return
	}
	// Build the argv: an explicit argv wins; otherwise wrap code as a snippet.
	argv := body.Argv
	cwd := body.Cwd
	if len(argv) == 0 {
		argv = []string{"python3", "-u", "-c", body.Code}
		if cwd == "" {
			cwd = "/tmp"
		}
	} else if cwd == "" {
		cwd = executor.RepoWorkDir // repo commands default to the work dir
	}
	timeout := defaultTimeout
	if body.TimeoutMS > 0 {
		timeout = min(time.Duration(body.TimeoutMS)*time.Millisecond, maxTimeout)
	}

	if !s.admit() {
		s.tooBusy(w)
		return
	}
	e := s.registry.New()
	go s.runInSession(e, rec, argv, cwd, timeout)
	writeJSON(w, http.StatusAccepted, map[string]string{"executionId": e.ID})
}

// runInSession mirrors run(), but execs into a live session instead of creating
// a throwaway container.
func (s *Server) runInSession(e *Execution, rec *SessionRecord, argv []string, cwd string, timeout time.Duration) {
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
	res, err := rec.sess.ExecArgv(context.Background(), argv, cwd, timeout, out)
	<-published
	if err == nil {
		s.stats.Record(res.StartupMS, res.DurationMS)
	} else {
		log.Printf("session exec %s failed: %v", e.ID, err)
	}
	e.finish(res, err)
}

// handleSessionUpload accepts a repo — either a zip (Content-Type
// application/zip) or a JSON {files:[{path,data(base64)}]} — validates it, and
// populates the session's work dir. Option C: fully offline.
func (s *Server) handleSessionUpload(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	var files []executor.RepoFile
	var label string
	var err error

	body := http.MaxBytesReader(w, r.Body, executor.MaxRepoBytes*2) // room for base64/zip overhead
	if strings.Contains(r.Header.Get("Content-Type"), "application/zip") {
		var data []byte
		data, err = io.ReadAll(body)
		if err == nil {
			files, err = executor.FilesFromZip(data)
			label = "zip"
		}
	} else {
		var req struct {
			Label string `json:"label"`
			Files []struct {
				Path string `json:"path"`
				Data string `json:"data"` // base64
			} `json:"files"`
		}
		if err = json.NewDecoder(body).Decode(&req); err == nil {
			label = req.Label
			for _, f := range req.Files {
				var raw []byte
				raw, err = base64.StdEncoding.DecodeString(f.Data)
				if err != nil {
					break
				}
				files = append(files, executor.RepoFile{Path: f.Path, Data: raw})
			}
		}
	}
	if err != nil {
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// UploadSource applies the same validation the git path would.
	validated, err := executor.UploadSource{Files: files, Label: label}.Fetch(r.Context())
	if err != nil {
		http.Error(w, "invalid repo: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := rec.sess.Populate(r.Context(), validated); err != nil {
		http.Error(w, "populate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rec.SetRepo(validated, label)
	rec.touch()
	writeJSON(w, http.StatusOK, map[string]any{
		"files": rec.RepoPaths(),
		"count": len(validated),
	})
}

// handleSessionFiles lists the repo tree, or returns one file's content when
// ?path= is given.
func (s *Server) handleSessionFiles(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if p := r.URL.Query().Get("path"); p != "" {
		data, ok := rec.RepoFile(p)
		if !ok {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": p, "content": string(data)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": rec.RepoPaths()})
}

// handleSessionDeleteFile removes one repo file (?path=).
func (s *Server) handleSessionDeleteFile(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing ?path=", http.StatusBadRequest)
		return
	}
	if err := rec.sess.RemoveFile(r.Context(), path); err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusBadRequest)
		return
	}
	rec.DropRepoFile(path)
	rec.touch()
	w.WriteHeader(http.StatusNoContent)
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
