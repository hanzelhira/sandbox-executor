package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

// SessionRecord tracks one live sandbox and its usage, so the reaper can find
// idle ones and the API can list them.
type SessionRecord struct {
	ID        string
	sess      executor.Session
	CreatedAt time.Time

	mu       sync.Mutex
	lastUsed time.Time
	runs     int
	busy     bool // an exec is in flight; do not reap
	// repo holds the file set uploaded to this session, so the UI can render a
	// tree and open files without exec-ing find/cat. It reflects what was
	// uploaded/edited, not files the running code generates.
	repo      map[string][]byte
	repoLabel string
}

// SetRepo records the uploaded file set (paths already validated).
func (r *SessionRecord) SetRepo(files []executor.RepoFile, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repo == nil {
		r.repo = map[string][]byte{}
	}
	for _, f := range files {
		r.repo[f.Path] = f.Data
	}
	if label != "" {
		r.repoLabel = label
	}
}

// RepoPaths returns the sorted file list for the tree.
func (r *SessionRecord) RepoPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	paths := make([]string, 0, len(r.repo))
	for p := range r.repo {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// RepoFile returns one file's bytes.
func (r *SessionRecord) RepoFile(path string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.repo[path]
	return b, ok
}

func (r *SessionRecord) touch() {
	r.mu.Lock()
	r.lastUsed = time.Now()
	r.runs++
	r.mu.Unlock()
}

func (r *SessionRecord) setBusy(b bool) {
	r.mu.Lock()
	r.busy = b
	r.mu.Unlock()
}

func (r *SessionRecord) snapshot() SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return SessionInfo{
		ID:         r.ID,
		CreatedAt:  r.CreatedAt,
		LastUsedMS: time.Since(r.lastUsed).Milliseconds(),
		Runs:       r.runs,
	}
}

// SessionInfo is the wire form for listing.
type SessionInfo struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedMS int64     `json:"idleMs"`
	Runs       int       `json:"runs"`
	Healthy    bool      `json:"healthy"`
}

// SessionRegistry owns the lifecycle of live sandboxes: creation under a cap,
// lookup, explicit destroy, an idle reaper, and destroy-all on shutdown.
type SessionRegistry struct {
	exec    executor.SessionExecutor
	maxLive int
	idleTTL time.Duration

	mu       sync.Mutex
	sessions map[string]*SessionRecord

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewSessionRegistry(exec executor.SessionExecutor, maxLive int, idleTTL time.Duration) *SessionRegistry {
	if maxLive < 1 {
		maxLive = 1
	}
	sr := &SessionRegistry{
		exec:     exec,
		maxLive:  maxLive,
		idleTTL:  idleTTL,
		sessions: make(map[string]*SessionRecord),
		stop:     make(chan struct{}),
	}
	if idleTTL > 0 {
		sr.wg.Add(1)
		go sr.reapLoop()
	}
	return sr
}

var ErrAtCapacity = errSentinel("session limit reached")
var ErrNoSuchSession = errSentinel("no such session")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// Create starts a new sandbox, refusing past the live cap.
func (sr *SessionRegistry) Create(ctx context.Context, spec executor.SessionSpec) (*SessionRecord, error) {
	sr.mu.Lock()
	if len(sr.sessions) >= sr.maxLive {
		sr.mu.Unlock()
		return nil, ErrAtCapacity
	}
	sr.mu.Unlock()

	// Create the container outside the lock (it does IO), then register it.
	sess, err := sr.exec.CreateSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	rec := &SessionRecord{
		ID:        hex.EncodeToString(b),
		sess:      sess,
		CreatedAt: time.Now(),
		lastUsed:  time.Now(),
	}

	sr.mu.Lock()
	// Re-check under lock: a racing Create could have filled the last slot.
	if len(sr.sessions) >= sr.maxLive {
		sr.mu.Unlock()
		_ = sess.Destroy(context.Background())
		return nil, ErrAtCapacity
	}
	sr.sessions[rec.ID] = rec
	sr.mu.Unlock()
	return rec, nil
}

func (sr *SessionRegistry) Get(id string) (*SessionRecord, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	r, ok := sr.sessions[id]
	return r, ok
}

func (sr *SessionRegistry) Destroy(id string) error {
	sr.mu.Lock()
	rec, ok := sr.sessions[id]
	if ok {
		delete(sr.sessions, id)
	}
	sr.mu.Unlock()
	if !ok {
		return ErrNoSuchSession
	}
	return rec.sess.Destroy(context.Background())
}

func (sr *SessionRegistry) List() []SessionInfo {
	sr.mu.Lock()
	recs := make([]*SessionRecord, 0, len(sr.sessions))
	for _, r := range sr.sessions {
		recs = append(recs, r)
	}
	sr.mu.Unlock()

	out := make([]SessionInfo, 0, len(recs))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, r := range recs {
		info := r.snapshot()
		info.Healthy = r.sess.Healthy(ctx)
		out = append(out, info)
	}
	return out
}

// reapLoop destroys sessions that have been idle past the TTL. Without it,
// abandoned sessions leak containers forever.
func (sr *SessionRegistry) reapLoop() {
	defer sr.wg.Done()
	ticker := time.NewTicker(sr.idleTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-sr.stop:
			return
		case <-ticker.C:
			sr.reapOnce()
		}
	}
}

func (sr *SessionRegistry) reapOnce() {
	now := time.Now()
	var doomed []*SessionRecord
	sr.mu.Lock()
	for id, r := range sr.sessions {
		r.mu.Lock()
		idle := now.Sub(r.lastUsed)
		busy := r.busy
		r.mu.Unlock()
		if !busy && idle > sr.idleTTL {
			doomed = append(doomed, r)
			delete(sr.sessions, id)
		}
	}
	sr.mu.Unlock()
	for _, r := range doomed {
		_ = r.sess.Destroy(context.Background())
	}
}

// Shutdown stops the reaper and destroys every live session, so the process
// never exits leaving orphaned containers behind.
func (sr *SessionRegistry) Shutdown() {
	close(sr.stop)
	sr.wg.Wait()
	sr.mu.Lock()
	recs := make([]*SessionRecord, 0, len(sr.sessions))
	for _, r := range sr.sessions {
		recs = append(recs, r)
	}
	sr.sessions = map[string]*SessionRecord{}
	sr.mu.Unlock()
	for _, r := range recs {
		_ = r.sess.Destroy(context.Background())
	}
}

func (sr *SessionRegistry) Len() int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return len(sr.sessions)
}
