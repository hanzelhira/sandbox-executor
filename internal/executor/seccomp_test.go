package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeccompProfileIsWellFormed(t *testing.T) {
	raw, n, err := SeccompProfileJSON()
	if err != nil {
		t.Fatal(err)
	}
	var p seccompProfile
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("profile is not valid JSON: %v", err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, want SCMP_ACT_ERRNO — an allowlist must deny by default",
			p.DefaultAction)
	}
	if n < 50 {
		t.Errorf("allowlist has only %d syscalls; CPython needs more than that", n)
	}
	// A "tightened" profile that permits most of the kernel is not tightened.
	// Linux has ~350 syscalls; Docker's default permits roughly 300.
	if n > 250 {
		t.Errorf("allowlist has %d syscalls — too close to Docker's default to be worth it", n)
	}

	// The denied list must not contradict the allowlist. Without this, a
	// syscall added for convenience could silently re-enable something the
	// documentation claims is blocked.
	allowed := map[string]bool{}
	for _, s := range AllowedSyscalls() {
		allowed[s] = true
	}
	for _, denied := range DeniedSyscalls() {
		if allowed[denied] {
			t.Errorf("%q appears in both the allowlist and the denied list", denied)
		}
	}
}

// callSyscall runs a raw syscall by number via ctypes and reports the result,
// so tests can distinguish "blocked by seccomp" (EPERM from the filter) from
// "allowed but refused" (a different errno from the kernel).
const syscallProbe = `
import ctypes, ctypes.util, errno
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
def call(nr, *args):
    ctypes.set_errno(0)
    rc = libc.syscall(ctypes.c_long(nr), *[ctypes.c_long(a) for a in args])
    return rc, ctypes.get_errno()
`

// Syscall numbers differ per architecture; these are aarch64 (the dev host).
// The test skips rather than fails elsewhere, since a wrong number would test
// nothing useful.
const (
	aarch64NRPtrace = 117
	aarch64NRMount  = 40
)

func TestSeccompBlocksDangerousSyscalls(t *testing.T) {
	d := newTestExecutor(t)
	if arch := hostArch(t, d); arch != "aarch64" {
		t.Skipf("syscall numbers in this test are aarch64; host is %s", arch)
	}

	code := syscallProbe + `
import errno
for name, nr in (("ptrace", 117), ("mount", 40)):
    rc, err = call(nr, 0, 0, 0, 0)
    print(f"{name}: rc={rc} errno={errno.errorcode.get(err, err)}")
`
	stdout, stderr, res := run(t, d, ExecRequest{
		Language: "python", Code: code,
		Limits: DefaultLimits(), Timeout: 20 * time.Second,
	})
	if res.ExitCode != 0 {
		t.Fatalf("probe failed: exit %d stderr=%s", res.ExitCode, truncate(stderr, 300))
	}
	// EPERM is what our SCMP_ACT_ERRNO default returns. A syscall that ran
	// would report a different errno (or succeed).
	for _, name := range []string{"ptrace", "mount"} {
		line := lineFor(stdout, name)
		if !strings.Contains(line, "EPERM") {
			t.Errorf("%s was not blocked by seccomp: %q", name, line)
		}
	}
	t.Logf("blocked syscalls:\n%s", strings.TrimSpace(stdout))
}

// Allowing setuid/setgid in the filter must not let the sandbox change user:
// capabilities are the separate gate that stops it.
func TestSeccompUserSwitchStillBlocked(t *testing.T) {
	d := newTestExecutor(t)
	stdout, _, _ := run(t, d, ExecRequest{
		Language: "python",
		Code: `
import os
print("uid before:", os.getuid())
try:
    os.setuid(0)
    print("ESCALATED to uid", os.getuid())
except PermissionError as e:
    print("setuid(0) refused:", e.__class__.__name__)
`,
		Limits: DefaultLimits(), Timeout: 20 * time.Second,
	})
	if strings.Contains(stdout, "ESCALATED") {
		t.Fatalf("privilege escalation succeeded: %q", stdout)
	}
	if !strings.Contains(stdout, "refused") {
		t.Errorf("unexpected output: %q", stdout)
	}
}

// The profile must not break ordinary Python: threads, subprocess, file IO,
// and imports all exercise syscalls the allowlist has to cover.
func TestSeccompAllowsNormalPython(t *testing.T) {
	d := newTestExecutor(t)
	stdout, stderr, res := run(t, d, ExecRequest{
		Language: "python",
		Code: `
import json, math, os, re, subprocess, tempfile, threading, datetime

with tempfile.NamedTemporaryFile("w", dir="/tmp", delete=False) as f:
    f.write("x" * 1000); path = f.name
print("file:", len(open(path).read()))

results = []
t = threading.Thread(target=lambda: results.append("thread ok"))
t.start(); t.join()
print(results[0])

print("subprocess:", subprocess.run(["echo", "hi"], capture_output=True).stdout.decode().strip())
print("json:", json.dumps({"pi": round(math.pi, 3)}))
print("re:", bool(re.match(r"^a+$", "aaa")))
print("time:", isinstance(datetime.datetime.now().year, int))
`,
		Limits: DefaultLimits(), Timeout: 30 * time.Second,
	})
	if res.ExitCode != 0 {
		t.Fatalf("normal Python broke under the profile: exit %d\nstderr: %s",
			res.ExitCode, truncate(stderr, 500))
	}
	for _, want := range []string{"file: 1000", "thread ok", "subprocess: hi", "json:", "re: True", "time: True"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in output:\n%s", want, stdout)
		}
	}
}

func hostArch(t *testing.T, d *DockerExecutor) string {
	t.Helper()
	info, err := d.cli.Info(context.Background())
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	return info.Architecture
}

func lineFor(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return l
		}
	}
	return ""
}
