package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// These tests need a Docker daemon and the python:3.12-slim image.
// Skipped automatically under -short.

const testImage = "python:3.12-slim"

func newTestExecutor(t *testing.T) *DockerExecutor {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a Docker daemon")
	}
	d, err := NewDockerExecutor(testImage)
	if err != nil {
		t.Skipf("no Docker daemon: %v", err)
	}
	if _, err := d.cli.Ping(context.Background()); err != nil {
		t.Skipf("no Docker daemon: %v", err)
	}
	return d
}

// run executes code and collects all output, so tests can assert on both
// the streamed text and the final result.
func run(t *testing.T, d *DockerExecutor, req ExecRequest) (stdout, stderr string, res ExecResult) {
	t.Helper()
	out := make(chan OutputChunk, 256)
	var sb, eb strings.Builder
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for c := range out {
			if c.Stream == "stderr" {
				eb.Write(c.Data)
			} else {
				sb.Write(c.Data)
			}
		}
	}()
	res, err := d.Execute(context.Background(), req, out)
	<-collected
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return sb.String(), eb.String(), res
}

func TestHelloWorld(t *testing.T) {
	d := newTestExecutor(t)
	stdout, _, res := run(t, d, ExecRequest{
		Language: "python",
		Code:     `print("hello sandbox")`,
		Limits:   DefaultLimits(),
		Timeout:  30 * time.Second,
	})
	if strings.TrimSpace(stdout) != "hello sandbox" {
		t.Errorf("stdout = %q", stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.TimedOut || res.OOMKilled {
		t.Errorf("unexpected flags: %+v", res)
	}
	if res.StartupMS <= 0 {
		t.Errorf("startup latency not measured: %d", res.StartupMS)
	}
	t.Logf("startup=%dms duration=%dms", res.StartupMS, res.DurationMS)
}

func TestStderrIsSeparate(t *testing.T) {
	d := newTestExecutor(t)
	stdout, stderr, res := run(t, d, ExecRequest{
		Language: "python",
		Code:     `import sys; print("out"); print("err", file=sys.stderr); sys.exit(3)`,
		Limits:   DefaultLimits(),
		Timeout:  30 * time.Second,
	})
	if strings.TrimSpace(stdout) != "out" {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.TrimSpace(stderr) != "err" {
		t.Errorf("stderr = %q", stderr)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3 (non-zero exit must be preserved)", res.ExitCode)
	}
}

// Attack 1: infinite loop must be killed by the timeout, and the whole
// container must go with it.
func TestInfiniteLoopIsTimedOut(t *testing.T) {
	d := newTestExecutor(t)
	start := time.Now()
	_, _, res := run(t, d, ExecRequest{
		Language: "python",
		Code:     `while True: pass`,
		Limits:   DefaultLimits(),
		Timeout:  3 * time.Second,
	})
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (result %+v)", res)
	}
	if elapsed > 15*time.Second {
		t.Errorf("took %v — kill is too slow", elapsed)
	}
	t.Logf("timed out after %v, result %+v", elapsed, res)
}

// Attack 2: a large allocation must be OOM-killed and reported distinctly
// from an ordinary non-zero exit.
func TestMemoryHogIsOOMKilled(t *testing.T) {
	d := newTestExecutor(t)
	limits := DefaultLimits()
	limits.MemoryBytes = 64 << 20 // 64 MiB
	_, _, res := run(t, d, ExecRequest{
		Language: "python",
		Code:     `x = bytearray(2 * 1024 * 1024 * 1024)`, // 2 GiB
		Limits:   limits,
		Timeout:  30 * time.Second,
	})
	if !res.OOMKilled {
		t.Errorf("OOMKilled = false, want true (result %+v)", res)
	}
	if res.ExitCode == 0 {
		t.Errorf("exit code = 0, want non-zero for an OOM kill")
	}
	t.Logf("OOM result: %+v", res)
}

// Attack 3: fork bomb must be contained by pids.max — the host stays healthy
// and the run ends by timeout rather than taking the machine down.
func TestForkBombIsContained(t *testing.T) {
	d := newTestExecutor(t)
	limits := DefaultLimits()
	limits.PIDsMax = 32
	start := time.Now()
	stdout, _, _ := run(t, d, ExecRequest{
		Language: "python",
		Code: `
import os
n = 0
while True:
    try:
        os.fork()
        n += 1
    except OSError as e:
        print("fork blocked after", n, "->", e, flush=True)
        break
`,
		Limits:  limits,
		Timeout: 10 * time.Second,
	})
	elapsed := time.Since(start)
	// Containment: we get control back and the daemon is alive...
	if elapsed > 20*time.Second {
		t.Errorf("fork bomb was not contained in time: %v", elapsed)
	}
	if _, err := d.cli.Ping(context.Background()); err != nil {
		t.Fatalf("docker daemon unhealthy after fork bomb: %v", err)
	}
	// ...and specifically because pids.max refused the fork, not for some
	// other reason. Without this the test could pass on an unrelated crash.
	if !strings.Contains(stdout, "fork blocked after") {
		t.Fatalf("no evidence pids.max stopped the forking; stdout=%q", truncate(stdout, 300))
	}
	t.Logf("contained in %v (limit %d pids): %s", elapsed, limits.PIDsMax, strings.TrimSpace(stdout))
}

// Attack 4: no network namespace means egress simply cannot happen.
func TestNetworkIsBlocked(t *testing.T) {
	d := newTestExecutor(t)
	stdout, _, res := run(t, d, ExecRequest{
		Language: "python",
		Code: `
import socket
socket.setdefaulttimeout(5)
try:
    socket.create_connection(("1.1.1.1", 53))
    print("NETWORK REACHABLE")
except Exception as e:
    print("blocked:", type(e).__name__)
`,
		Limits:  DefaultLimits(),
		Timeout: 20 * time.Second,
	})
	if strings.Contains(stdout, "NETWORK REACHABLE") {
		t.Errorf("network was reachable — isolation failed. stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "blocked:") {
		t.Errorf("unexpected output: %q (result %+v)", stdout, res)
	}
	t.Logf("network: %s", strings.TrimSpace(stdout))
}

// The rootfs must be read-only, with /tmp the only writable spot.
func TestFilesystemIsReadOnly(t *testing.T) {
	d := newTestExecutor(t)
	stdout, _, _ := run(t, d, ExecRequest{
		Language: "python",
		Code: `
for path in ("/evil", "/etc/evil", "/tmp/ok"):
    try:
        open(path, "w").write("x")
        print(path, "WRITABLE")
    except Exception as e:
        print(path, "blocked:", type(e).__name__)
`,
		Limits:  DefaultLimits(),
		Timeout: 20 * time.Second,
	})
	if strings.Contains(stdout, "/evil WRITABLE") || strings.Contains(stdout, "/etc/evil WRITABLE") {
		t.Errorf("rootfs was writable: %q", stdout)
	}
	if !strings.Contains(stdout, "/tmp/ok WRITABLE") {
		t.Errorf("/tmp should be writable scratch: %q", stdout)
	}
	t.Logf("filesystem:\n%s", strings.TrimSpace(stdout))
}

// Non-root user, so an escape lands as nobody.
func TestRunsAsNonRoot(t *testing.T) {
	d := newTestExecutor(t)
	stdout, _, _ := run(t, d, ExecRequest{
		Language: "python",
		Code:     `import os; print("uid", os.getuid())`,
		Limits:   DefaultLimits(),
		Timeout:  20 * time.Second,
	})
	if !strings.Contains(stdout, "uid 1000") {
		t.Errorf("want uid 1000, got %q", stdout)
	}
}

// No container may survive a run — timeout kills included.
func TestNoContainersLeak(t *testing.T) {
	d := newTestExecutor(t)
	before := countContainers(t, d)
	run(t, d, ExecRequest{
		Language: "python", Code: `while True: pass`,
		Limits: DefaultLimits(), Timeout: 2 * time.Second,
	})
	time.Sleep(500 * time.Millisecond) // removal is deferred
	if after := countContainers(t, d); after > before {
		t.Errorf("leaked containers: %d -> %d", before, after)
	}
}

func countContainers(t *testing.T, d *DockerExecutor) int {
	t.Helper()
	list, err := d.cli.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	return len(list)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
