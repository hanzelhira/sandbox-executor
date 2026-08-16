package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newTestSession(t *testing.T, limits ResourceLimits) Session {
	t.Helper()
	d := newTestExecutor(t)
	sess, err := d.CreateSession(context.Background(), SessionSpec{Language: "python", Limits: limits})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Destroy(context.Background()) })
	return sess
}

func execInSession(t *testing.T, s Session, code string, timeout time.Duration) (string, string, ExecResult) {
	t.Helper()
	out := make(chan OutputChunk, 256)
	var sb, eb strings.Builder
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for c := range out {
			if c.Stream == "stderr" {
				eb.WriteString(string(c.Data))
			} else {
				sb.WriteString(string(c.Data))
			}
		}
	}()
	res, err := s.Exec(context.Background(), code, timeout, out)
	<-collected
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return sb.String(), eb.String(), res
}

// The headline property: files written in one run are visible in the next,
// because the container (and its tmpfs) lives on between execs.
func TestSessionStatePersistsAcrossRuns(t *testing.T) {
	s := newTestSession(t, DefaultLimits())

	_, _, r1 := execInSession(t, s, `open("/tmp/state","w").write("41")`, 15*time.Second)
	if r1.ExitCode != 0 {
		t.Fatalf("write run failed: %+v", r1)
	}
	stdout, _, r2 := execInSession(t, s, `print(int(open("/tmp/state").read()) + 1)`, 15*time.Second)
	if r2.ExitCode != 0 {
		t.Fatalf("read run failed: %+v", r2)
	}
	if strings.TrimSpace(stdout) != "42" {
		t.Errorf("state did not persist: got %q, want 42", strings.TrimSpace(stdout))
	}
	// The per-run startup should be much cheaper than a cold container create.
	t.Logf("second run startup=%dms (exec, not container-create)", r2.StartupMS)
}

// Separate sessions must not share state — isolation between sessions is
// absolute even though it is relaxed within one.
func TestSessionsAreIsolatedFromEachOther(t *testing.T) {
	d := newTestExecutor(t)
	a, err := d.CreateSession(context.Background(), SessionSpec{Language: "python", Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Destroy(context.Background())
	b, err := d.CreateSession(context.Background(), SessionSpec{Language: "python", Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Destroy(context.Background())

	execInSession(t, a, `open("/tmp/secret","w").write("in-a")`, 15*time.Second)
	stdout, _, _ := execInSession(t, b, `
import os
print("leaked" if os.path.exists("/tmp/secret") else "isolated")
`, 15*time.Second)
	if strings.TrimSpace(stdout) != "isolated" {
		t.Errorf("session B saw session A's file: %q", stdout)
	}
}

// A timeout must kill the run but leave the session usable — the whole point of
// sessions is that state survives a bad run.
func TestSessionTimeoutKillsRunNotSession(t *testing.T) {
	s := newTestSession(t, DefaultLimits())

	// Seed state, then a run that never terminates.
	execInSession(t, s, `open("/tmp/keep","w").write("here")`, 15*time.Second)
	_, _, r := execInSession(t, s, `while True: pass`, 2*time.Second)
	if !r.TimedOut {
		t.Errorf("expected TimedOut, got %+v", r)
	}

	// The session must still work, and the earlier state must still be there.
	if !s.Healthy(context.Background()) {
		t.Fatal("session died from a run timeout — it should survive")
	}
	stdout, _, r3 := execInSession(t, s, `print(open("/tmp/keep").read())`, 15*time.Second)
	if r3.ExitCode != 0 || strings.TrimSpace(stdout) != "here" {
		t.Errorf("session unusable or state lost after timeout: exit=%d out=%q", r3.ExitCode, stdout)
	}
}

// Isolation controls still hold inside an exec.
func TestSessionExecIsStillIsolated(t *testing.T) {
	s := newTestSession(t, DefaultLimits())
	stdout, _, _ := execInSession(t, s, `
import os, socket
socket.setdefaulttimeout(3)
print("uid", os.getuid())
try:
    open("/etc/evil","w").write("x"); print("rootfs WRITABLE")
except Exception as e:
    print("rootfs blocked:", type(e).__name__)
try:
    socket.create_connection(("1.1.1.1", 53)); print("NET REACHABLE")
except Exception as e:
    print("net blocked:", type(e).__name__)
`, 20*time.Second)
	if !strings.Contains(stdout, "uid 1000") {
		t.Errorf("exec not running as non-root: %q", stdout)
	}
	if strings.Contains(stdout, "WRITABLE") || strings.Contains(stdout, "REACHABLE") {
		t.Errorf("isolation broken inside session exec: %q", stdout)
	}
}

// Destroy removes the container; a destroyed session refuses further execs.
func TestSessionDestroy(t *testing.T) {
	d := newTestExecutor(t)
	before := countContainers(t, d)
	s, err := d.CreateSession(context.Background(), SessionSpec{Language: "python", Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	execInSession(t, s, `print("alive")`, 15*time.Second)
	if err := s.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if after := countContainers(t, d); after > before {
		t.Errorf("session container leaked: %d -> %d", before, after)
	}
	// Further execs must fail rather than hang.
	out := make(chan OutputChunk, 4)
	if _, err := s.Exec(context.Background(), `print(1)`, 5*time.Second, out); err == nil {
		t.Error("exec on a destroyed session should error")
	}
}
