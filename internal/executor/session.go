package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// SessionExecutor is implemented by backends that support long-lived sandboxes:
// create a container once, run code in it repeatedly with state persisting, then
// destroy it. It is separate from Executor so a backend can offer one-shot
// execution without sessions, and so the core interface stays minimal.
type SessionExecutor interface {
	CreateSession(ctx context.Context, spec SessionSpec) (Session, error)
}

type SessionSpec struct {
	Language string
	Limits   ResourceLimits // applied to the container, i.e. per session
}

// Session is a live sandbox. Exec runs code in it; state (globals via a fresh
// interpreter do NOT persist — files in /tmp do) survives between Execs.
type Session interface {
	ID() string
	Exec(ctx context.Context, code string, timeout time.Duration, out chan<- OutputChunk) (ExecResult, error)
	// ExecArgv runs an arbitrary argv in a working directory — the repo path.
	// No shell is involved, so there is no shell injection.
	ExecArgv(ctx context.Context, argv []string, cwd string, timeout time.Duration, out chan<- OutputChunk) (ExecResult, error)
	// Populate writes a validated file set into RepoWorkDir. It is additive:
	// calling it again overlays more files (used for single-file edits).
	Populate(ctx context.Context, files []RepoFile) error
	// RemoveFile deletes one repo-relative path from RepoWorkDir. The path is
	// validated the same way uploads are, so it cannot escape the work dir.
	RemoveFile(ctx context.Context, path string) error
	Healthy(ctx context.Context) bool
	Destroy(ctx context.Context) error
}

// CreateSession starts a locked-down container running `sleep infinity` as PID 1
// so the container outlives any single run. Same isolation as one-shot Execute
// (network none, read-only rootfs, tmpfs, caps dropped, seccomp, non-root); the
// resource limits now bound the whole session rather than a single run.
func (d *DockerExecutor) CreateSession(ctx context.Context, spec SessionSpec) (Session, error) {
	if spec.Language != "python" {
		return nil, fmt.Errorf("unsupported language %q", spec.Language)
	}
	limits := spec.Limits
	if limits.MemoryBytes == 0 {
		limits = DefaultLimits()
	}

	cfg := d.containerConfig([]string{"sleep", "infinity"})
	// sleep lives on the read-only rootfs, so no writable exec is needed here.
	resp, err := d.cli.ContainerCreate(ctx, cfg, d.hostConfig(limits), nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create session container: %w", err)
	}
	id := resp.ID
	if err := d.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = d.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
		cancel()
		return nil, fmt.Errorf("start session container: %w", err)
	}
	return &dockerSession{d: d, id: id}, nil
}

type dockerSession struct {
	d  *DockerExecutor
	id string

	mu    sync.Mutex
	dead  bool // set once the container is gone or wedged
	nRuns int
}

func (s *dockerSession) ID() string { return s.id }

// Exec runs a single Python snippet — sugar over ExecArgv for the scratch path.
func (s *dockerSession) Exec(ctx context.Context, code string, timeout time.Duration, out chan<- OutputChunk) (ExecResult, error) {
	return s.ExecArgv(ctx, []string{"python3", "-u", "-c", code}, "/tmp", timeout, out)
}

// ExecArgv runs an arbitrary argv as a new process inside the live container.
// A timeout kills the exec'd process, NOT the container, so one runaway run
// does not destroy the session and its accumulated state. No shell is spawned,
// so argv is passed straight to execve — shell injection is impossible.
func (s *dockerSession) ExecArgv(ctx context.Context, argv []string, cwd string, timeout time.Duration, out chan<- OutputChunk) (ExecResult, error) {
	defer close(out)

	if len(argv) == 0 {
		return ExecResult{}, fmt.Errorf("empty argv")
	}
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return ExecResult{}, fmt.Errorf("session %s is no longer usable", short(s.id))
	}
	s.nRuns++
	s.mu.Unlock()

	if cwd == "" {
		cwd = "/tmp"
	}
	created := time.Now()
	execCfg := container.ExecOptions{
		Cmd:          argv,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   cwd,
	}
	ec, err := s.d.cli.ContainerExecCreate(ctx, s.id, execCfg)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	runCtx := ctx
	var cancelRun context.CancelFunc
	if timeout > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, timeout)
		defer cancelRun()
	}

	attach, err := s.d.cli.ContainerExecAttach(runCtx, ec.ID, container.ExecStartOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	started := time.Now()
	startupMS := started.Sub(created).Milliseconds() // exec-create -> attached

	pump := startStreamPump(attach.Reader, attach.Conn, out)
	defer pump.finish()

	result := ExecResult{StartupMS: startupMS}

	// Poll the exec's state until it exits or the deadline fires. Unlike a
	// one-shot container there is no ContainerWait for an exec, so we inspect.
	done := s.waitExec(runCtx, ec.ID, &result)
	if !done {
		// Deadline (or cancel): kill just this process tree, leaving the
		// container — and the session's state — alive.
		s.killExec(ec.ID, &result, ctx)
	}
	result.DurationMS = time.Since(started).Milliseconds()

	// A run can leave the session unusable: an OOM may have killed PID 1, or a
	// fork bomb may have exhausted pids.max so nothing new can exec. Mark the
	// session dead so the registry stops routing to it.
	if !s.Healthy(context.Background()) {
		s.mu.Lock()
		s.dead = true
		s.mu.Unlock()
		result.OOMKilled = s.containerOOMKilled()
	}
	return result, nil
}

// waitExec blocks until the exec exits (returning true and setting ExitCode) or
// runCtx is done (returning false).
func (s *dockerSession) waitExec(runCtx context.Context, execID string, result *ExecResult) bool {
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return false
		case <-ticker.C:
			insp, err := s.d.cli.ContainerExecInspect(context.Background(), execID)
			if err != nil {
				result.ExitCode = -1
				return true
			}
			if !insp.Running {
				result.ExitCode = insp.ExitCode
				return true
			}
		}
	}
}

// killExec terminates a timed-out exec's process tree without touching the
// container. The exec'd PID is known to the daemon; `kill -9 -PID` on the
// process group cleans up children too.
func (s *dockerSession) killExec(execID string, result *ExecResult, parent context.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if insp, err := s.d.cli.ContainerExecInspect(ctx, execID); err == nil && insp.Pid > 0 {
		// Run a helper exec that kills the target pid's process group. The
		// session container has no shell guarantees, so use kill(1) directly.
		killCfg := container.ExecOptions{Cmd: []string{"kill", "-9", fmt.Sprint(insp.Pid)}}
		if kec, err := s.d.cli.ContainerExecCreate(ctx, s.id, killCfg); err == nil {
			_ = s.d.cli.ContainerExecStart(ctx, kec.ID, container.ExecStartOptions{})
		}
	}
	result.TimedOut = parent.Err() == nil
	result.ExitCode = -1
}

// Healthy reports whether the container is still running and can accept execs.
func (s *dockerSession) Healthy(ctx context.Context) bool {
	insp, err := s.d.cli.ContainerInspect(ctx, s.id)
	if err != nil || insp.State == nil {
		return false
	}
	return insp.State.Running
}

func (s *dockerSession) containerOOMKilled() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	insp, err := s.d.cli.ContainerInspect(ctx, s.id)
	if err != nil || insp.State == nil {
		return false
	}
	return insp.State.OOMKilled
}

// unpackScript reads a tar from stdin and extracts it into RepoWorkDir. Run
// inside the container via exec, so it writes to the writable tmpfs — Docker's
// CopyToContainer refuses a read-only-rootfs container even for tmpfs. filter=
// "data" (Python 3.12) rejects absolute paths, traversal, and special files as
// defense in depth on top of ValidateRepo.
const unpackScript = `import sys, os, tarfile
d = "` + RepoWorkDir + `"
os.makedirs(d, exist_ok=True)
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as tf:
    tf.extractall(d, filter="data")
`

// Populate writes a validated file set into RepoWorkDir by piping a tar into an
// unpacker process inside the container. The tmpfs at /tmp is writable, so this
// succeeds where CopyToContainer does not. Additive: re-calling overlays files,
// which is how single-file edits are pushed back.
func (s *dockerSession) Populate(ctx context.Context, files []RepoFile) error {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return fmt.Errorf("session %s is no longer usable", short(s.id))
	}
	s.mu.Unlock()

	tarBuf, err := TarRepo(files)
	if err != nil {
		return fmt.Errorf("build repo tar: %w", err)
	}
	return s.execWithInput(ctx, []string{"python3", "-c", unpackScript}, tarBuf.Bytes())
}

// execWithInput runs an argv inside the container, feeds it stdin, and waits
// for it to exit non-streaming. Used for populate/edit where we need the input
// side and only care about success. It reads stdout/stderr to drain the pipe.
func (s *dockerSession) execWithInput(ctx context.Context, argv []string, stdin []byte) error {
	ec, err := s.d.cli.ContainerExecCreate(ctx, s.id, container.ExecOptions{
		Cmd:          argv,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	attach, err := s.d.cli.ContainerExecAttach(ctx, ec.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	// Drain output in the background so the process never blocks on a full pipe.
	var stderr bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = stdcopy.StdCopy(io.Discard, &stderr, attach.Reader)
	}()

	if _, err := attach.Conn.Write(stdin); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	// Half-close stdin so the reader gets EOF and exits.
	if cw, ok := attach.Conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	<-drained

	insp, err := s.d.cli.ContainerExecInspect(ctx, ec.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("unpack failed (exit %d): %s", insp.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RemoveFile deletes one path from the work dir via `rm -f` (argv, no shell,
// so the validated path can't be turned into an injection). Validation is the
// same guard uploads use, so nothing outside RepoWorkDir can be touched.
func (s *dockerSession) RemoveFile(ctx context.Context, path string) error {
	clean, err := CleanRepoPath(path)
	if err != nil {
		return err
	}
	full := RepoWorkDir + "/" + clean
	ec, err := s.d.cli.ContainerExecCreate(ctx, s.id, container.ExecOptions{
		Cmd: []string{"rm", "-f", full},
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	if err := s.d.cli.ContainerExecStart(ctx, ec.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("exec start: %w", err)
	}
	return nil
}

func (s *dockerSession) Destroy(ctx context.Context) error {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.d.cli.ContainerRemove(rmCtx, s.id, container.RemoveOptions{Force: true})
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// compile-time check that stdcopy is still the demux used, so streamPump and
// session Exec cannot drift apart on framing.
var _ = stdcopy.StdCopy
