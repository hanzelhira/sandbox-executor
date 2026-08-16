package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerExecutor runs each execution in a fresh, locked-down container.
//
// Isolation knobs (all set on every container):
//   - memory.max, cpu.max, pids.max via cgroups v2
//   - NetworkMode "none" (no exfiltration / lateral movement)
//   - read-only rootfs + tmpfs /tmp (rw,64m,noexec)
//   - all capabilities dropped, no-new-privileges, non-root user
//   - Docker's default seccomp profile (custom profile is a Week 2 item)
//   - context timeout -> the whole container is killed, not just the process
type DockerExecutor struct {
	cli   *client.Client
	image string
	// seccompProfile is the SecurityOpt value: our allowlist profile, or
	// "seccomp=unconfined"/Docker's default when comparing.
	seccompProfile string
	// runtime is the OCI runtime name. Empty means Docker's default (runc);
	// this is the field a gVisor backend would set to "runsc".
	runtime string
	// SyscallsAllowed is how many syscalls the active profile permits (0 when
	// using Docker's default, which is a denylist and not counted here).
	SyscallsAllowed int
}

// SeccompMode selects which syscall filter a new executor applies.
type SeccompMode int

const (
	// SeccompCustom is the default: an allowlist built from observed CPython
	// behaviour. Everything not on it returns EPERM.
	SeccompCustom SeccompMode = iota
	// SeccompDockerDefault uses Docker's built-in denylist. Kept so the two
	// can be measured against each other.
	SeccompDockerDefault
	// SeccompOff disables filtering entirely. Only for demonstrating what the
	// filter is worth — never for running untrusted code.
	SeccompOff
)

func NewDockerExecutor(image string) (*DockerExecutor, error) {
	return NewDockerExecutorWithSeccomp(image, SeccompCustom)
}

func NewDockerExecutorWithSeccomp(image string, mode SeccompMode) (*DockerExecutor, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	d := &DockerExecutor{cli: cli, image: image}
	switch mode {
	case SeccompCustom:
		profile, n, err := SeccompProfileJSON()
		if err != nil {
			return nil, err
		}
		d.seccompProfile, d.SyscallsAllowed = "seccomp="+profile, n
	case SeccompOff:
		d.seccompProfile = "seccomp=unconfined"
	case SeccompDockerDefault:
		d.seccompProfile = "" // omit the option; Docker applies its default
	}
	return d, nil
}

func (d *DockerExecutor) Execute(ctx context.Context, req ExecRequest, out chan<- OutputChunk) (ExecResult, error) {
	defer close(out)

	if req.Language != "python" {
		return ExecResult{}, fmt.Errorf("unsupported language %q", req.Language)
	}
	limits := req.Limits
	if limits.MemoryBytes == 0 {
		limits = DefaultLimits()
	}

	created := time.Now()
	// -u: unbuffered stdout/stderr so streaming is live, not batched.
	cmd := []string{"python3", "-u", "-c", req.Code}
	resp, err := d.cli.ContainerCreate(ctx,
		d.containerConfig(cmd),
		d.hostConfig(limits),
		nil, nil, "")
	if err != nil {
		return ExecResult{}, fmt.Errorf("create container: %w", err)
	}
	id := resp.ID
	// Cleanup must survive a canceled request context.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
	}()

	// Attach before starting so the first output byte is never missed.
	attach, err := d.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("attach: %w", err)
	}
	defer attach.Close()

	runCtx := ctx
	var cancelRun context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, req.Timeout)
		defer cancelRun()
	}

	if err := d.cli.ContainerStart(runCtx, id, container.StartOptions{}); err != nil {
		return ExecResult{}, fmt.Errorf("start: %w", err)
	}
	started := time.Now()
	startupMS := started.Sub(created).Milliseconds()

	// The caller closes out once Execute returns, so the pump goroutine MUST be
	// finished by then — a send on a closed channel is an unrecoverable panic.
	pump := startStreamPump(attach.Reader, attach.Conn, out)
	defer pump.finish()

	waitCh, waitErrCh := d.cli.ContainerWait(runCtx, id, container.WaitConditionNotRunning)

	result := ExecResult{StartupMS: startupMS}
	select {
	case w := <-waitCh:
		result.ExitCode = int(w.StatusCode)
	case err := <-waitErrCh:
		if runCtx.Err() == nil {
			return result, fmt.Errorf("wait: %w", err)
		}
		d.killContainer(id, &result, ctx)
	case <-runCtx.Done():
		d.killContainer(id, &result, ctx)
	}
	result.DurationMS = time.Since(started).Milliseconds()

	// OOM must be surfaced distinctly from a plain non-zero exit.
	inspCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if insp, err := d.cli.ContainerInspect(inspCtx, id); err == nil && insp.State != nil {
		result.OOMKilled = insp.State.OOMKilled
	}
	return result, nil
}

func (d *DockerExecutor) securityOpts() []string {
	opts := []string{"no-new-privileges"}
	if d.seccompProfile != "" {
		opts = append(opts, d.seccompProfile)
	}
	return opts
}

// containerConfig and hostConfig build the identical lockdown used by both
// one-shot Execute and long-lived sessions — the only difference is the
// command, so the isolation posture can never silently diverge between them.
func (d *DockerExecutor) containerConfig(cmd []string) *container.Config {
	return &container.Config{
		Image:           d.image,
		Cmd:             cmd,
		User:            "1000:1000",
		NetworkDisabled: true,
		WorkingDir:      "/tmp",
	}
}

func (d *DockerExecutor) hostConfig(limits ResourceLimits) *container.HostConfig {
	return &container.HostConfig{
		Runtime:        d.runtime, // "" = runc; a gVisor backend sets "runsc"
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,size=64m,noexec"},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    d.securityOpts(),
		Resources: container.Resources{
			Memory:     limits.MemoryBytes,
			MemorySwap: limits.MemoryBytes, // no swap escape hatch
			NanoCPUs:   int64(limits.CPUQuota * 1e9),
			PidsLimit:  &limits.PIDsMax,
		},
		AutoRemove: false, // we inspect (OOMKilled) before removing ourselves
	}
}

// killContainer handles a timed-out (or caller-canceled) run: kill the whole
// container so nothing lingers, and mark the result accordingly.
func (d *DockerExecutor) killContainer(id string, result *ExecResult, parent context.Context) {
	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.cli.ContainerKill(killCtx, id, "SIGKILL")
	result.TimedOut = parent.Err() == nil // parent still live => it was our deadline
	result.ExitCode = -1
}

type chunkWriter struct {
	stream string
	out    chan<- OutputChunk
	abort  <-chan struct{}
}

var errAborted = errors.New("output consumer gone")

func (w chunkWriter) Write(p []byte) (int, error) {
	// Copy: io.Writer's contract lets the caller reuse p immediately, and
	// StdCopy does. Sending p itself would hand the consumer memory that gets
	// overwritten under them.
	data := make([]byte, len(p))
	copy(data, p)
	select {
	case w.out <- OutputChunk{Stream: w.stream, Data: data, TS: time.Now()}:
		return len(p), nil
	case <-w.abort:
		// Returning an error makes StdCopy give up, so we never block forever
		// on a consumer that stopped reading.
		return 0, errAborted
	}
}

var _ io.Writer = chunkWriter{}

// streamPump demuxes a Docker stdcopy stream into per-stream OutputChunks and
// guarantees its goroutine has stopped before the caller closes out — the same
// close-race protection Execute needs, shared with session Exec.
//
// Usage: start it, do the run, then call finish() (deferred) before returning.
// finish waits up to 2s for natural EOF, then forces the reader closed.
type streamPump struct {
	abort    chan struct{}
	copyDone chan struct{}
	closer   io.Closer
}

func startStreamPump(reader io.Reader, closer io.Closer, out chan<- OutputChunk) *streamPump {
	p := &streamPump{
		abort:    make(chan struct{}),
		copyDone: make(chan struct{}),
		closer:   closer,
	}
	go func() {
		defer close(p.copyDone)
		_, _ = stdcopy.StdCopy(
			chunkWriter{stream: "stdout", out: out, abort: p.abort},
			chunkWriter{stream: "stderr", out: out, abort: p.abort},
			reader,
		)
	}()
	return p
}

func (p *streamPump) finish() {
	select {
	case <-p.copyDone:
		return
	case <-time.After(2 * time.Second):
	}
	close(p.abort)
	_ = p.closer.Close()
	<-p.copyDone
}
