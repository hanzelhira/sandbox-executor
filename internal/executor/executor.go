// Package executor defines the backend-agnostic execution interface.
// DockerExecutor implements it in Week 1; gVisor/Firecracker backends
// implement the same interface later without touching the API or UI.
package executor

import (
	"context"
	"time"
)

type Executor interface {
	// Execute runs code and streams output via out until completion.
	// The implementation closes out when no more chunks will be sent.
	Execute(ctx context.Context, req ExecRequest, out chan<- OutputChunk) (ExecResult, error)
}

type ExecRequest struct {
	Language string // "python"
	Code     string
	Limits   ResourceLimits
	Timeout  time.Duration
}

type ResourceLimits struct {
	MemoryBytes int64   // -> memory.max
	CPUQuota    float64 // -> cpu.max (e.g. 0.5 = half a core)
	PIDsMax     int64   // -> pids.max (fork-bomb guard)
}

type OutputChunk struct {
	Stream string    `json:"stream"` // "stdout" | "stderr"
	Data   []byte    `json:"data"`
	TS     time.Time `json:"ts"`
}

type ExecResult struct {
	ExitCode   int   `json:"exitCode"`
	TimedOut   bool  `json:"timedOut"`
	OOMKilled  bool  `json:"oomKilled"`
	DurationMS int64 `json:"durationMs"`
	StartupMS  int64 `json:"startupMs"` // container create -> first byte executable
}

// DefaultLimits are applied when a request leaves limits unset.
func DefaultLimits() ResourceLimits {
	return ResourceLimits{
		MemoryBytes: 256 << 20, // 256 MiB
		CPUQuota:    0.5,
		PIDsMax:     64,
	}
}
