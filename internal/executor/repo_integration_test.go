package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A small multi-file repo populated into a session must run with imports across
// files working, in the repo's working directory.
func TestRepoPopulateAndRun(t *testing.T) {
	s := newTestSession(t, DefaultLimits())

	files, err := ValidateRepo([]RepoFile{
		{Path: "util.py", Data: []byte("def greet(n):\n    return f'hello {n}'\n")},
		{Path: "main.py", Data: []byte("from util import greet\nprint(greet('repo'))\n")},
		{Path: "sub/data.txt", Data: []byte("42")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Populate(context.Background(), files); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	// Run main.py from the repo root so the cross-file import resolves.
	stdout, stderr, res := execArgv(t, s, []string{"python3", "main.py"}, RepoWorkDir, 20*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("run failed: exit=%d stderr=%s", res.ExitCode, truncate(stderr, 300))
	}
	if strings.TrimSpace(stdout) != "hello repo" {
		t.Errorf("stdout = %q, want 'hello repo'", strings.TrimSpace(stdout))
	}

	// A nested data file must be readable relative to the work dir.
	stdout, _, res = execArgv(t, s, []string{"python3", "-c", "print(open('sub/data.txt').read())"}, RepoWorkDir, 15*time.Second)
	if res.ExitCode != 0 || strings.TrimSpace(stdout) != "42" {
		t.Errorf("nested file read failed: exit=%d out=%q", res.ExitCode, stdout)
	}
}

// Re-populating overlays an edited file, and the next run sees the new content.
func TestRepoEditPushback(t *testing.T) {
	s := newTestSession(t, DefaultLimits())
	v1, _ := ValidateRepo([]RepoFile{{Path: "app.py", Data: []byte("print('v1')")}})
	if err := s.Populate(context.Background(), v1); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ := execArgv(t, s, []string{"python3", "app.py"}, RepoWorkDir, 15*time.Second)
	if strings.TrimSpace(stdout) != "v1" {
		t.Fatalf("want v1, got %q", stdout)
	}

	v2, _ := ValidateRepo([]RepoFile{{Path: "app.py", Data: []byte("print('v2')")}})
	if err := s.Populate(context.Background(), v2); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ = execArgv(t, s, []string{"python3", "app.py"}, RepoWorkDir, 15*time.Second)
	if strings.TrimSpace(stdout) != "v2" {
		t.Errorf("edit not reflected: want v2, got %q", stdout)
	}
}

// A repo runs under the same isolation as a snippet: still no network, still
// non-root, still read-only outside /tmp.
func TestRepoRunStillIsolated(t *testing.T) {
	s := newTestSession(t, DefaultLimits())
	probe := "import os, socket\n" +
		"socket.setdefaulttimeout(3)\n" +
		"print('uid', os.getuid())\n" +
		"try:\n" +
		"    socket.create_connection(('1.1.1.1', 53)); print('NET OK')\n" +
		"except Exception as e:\n" +
		"    print('net blocked:', type(e).__name__)\n" +
		"try:\n" +
		"    open('/etc/x', 'w'); print('ROOTFS WRITABLE')\n" +
		"except Exception as e:\n" +
		"    print('rootfs blocked:', type(e).__name__)\n"
	files, _ := ValidateRepo([]RepoFile{{Path: "probe.py", Data: []byte(probe)}})
	s.Populate(context.Background(), files)
	stdout, _, _ := execArgv(t, s, []string{"python3", "probe.py"}, RepoWorkDir, 20*time.Second)
	if strings.Contains(stdout, "NET OK") || strings.Contains(stdout, "ROOTFS WRITABLE") {
		t.Errorf("isolation broken for repo run: %q", stdout)
	}
	if !strings.Contains(stdout, "uid 1000") {
		t.Errorf("repo not running as non-root: %q", stdout)
	}
}

func execArgv(t *testing.T, s Session, argv []string, cwd string, timeout time.Duration) (string, string, ExecResult) {
	t.Helper()
	out := make(chan OutputChunk, 256)
	var sb, eb strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range out {
			if c.Stream == "stderr" {
				eb.WriteString(string(c.Data))
			} else {
				sb.WriteString(string(c.Data))
			}
		}
	}()
	res, err := s.ExecArgv(context.Background(), argv, cwd, timeout, out)
	<-done
	if err != nil {
		t.Fatalf("ExecArgv: %v", err)
	}
	return sb.String(), eb.String(), res
}
