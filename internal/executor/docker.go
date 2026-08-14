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
}

func NewDockerExecutor(image string) (*DockerExecutor, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerExecutor{cli: cli, image: image}, nil
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
	resp, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image: d.image,
			// -u: unbuffered stdout/stderr so streaming is live, not batched.
			Cmd:             []string{"python3", "-u", "-c", req.Code},
			User:            "1000:1000",
			NetworkDisabled: true,
			WorkingDir:      "/tmp",
		},
		&container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": "rw,size=64m,noexec"},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:     limits.MemoryBytes,
				MemorySwap: limits.MemoryBytes, // no swap escape hatch
				NanoCPUs:   int64(limits.CPUQuota * 1e9),
				PidsLimit:  &limits.PIDsMax,
			},
			AutoRemove: false, // we inspect (OOMKilled) before removing ourselves
		},
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

	// Demux the attached stream into per-stream chunks on the out channel.
	// abort lets us unblock the writers if nobody is draining out.
	abort := make(chan struct{})
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = stdcopy.StdCopy(
			chunkWriter{stream: "stdout", out: out, abort: abort},
			chunkWriter{stream: "stderr", out: out, abort: abort},
			attach.Reader,
		)
	}()

	// The caller closes out once Execute returns, so this goroutine MUST be
	// finished by then — a send on a closed channel is an unrecoverable panic.
	// Give it a moment to drain naturally (container exit -> EOF -> return),
	// then force it: closing abort unblocks a stuck writer and closing the
	// connection makes StdCopy's read fail. Only then wait unconditionally.
	defer func() {
		select {
		case <-copyDone:
			return
		case <-time.After(2 * time.Second):
		}
		close(abort)
		attach.Close()
		<-copyDone
	}()

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
