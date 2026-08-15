package executor

import (
	"strings"
	"testing"
	"time"
)

// TestSeccompComparison is the measurement that justifies the custom profile:
// it runs the same syscall probe under Docker's default profile and under
// ours, and reports where they differ.
//
//	go test ./internal/executor -run TestSeccompComparison -v -count=1
func TestSeccompComparison(t *testing.T) {
	custom := newTestExecutor(t)
	if arch := hostArch(t, custom); arch != "aarch64" {
		t.Skipf("syscall numbers here are aarch64; host is %s", arch)
	}
	def, err := NewDockerExecutorWithSeccomp(testImage, SeccompDockerDefault)
	if err != nil {
		t.Fatalf("default-profile executor: %v", err)
	}

	// aarch64 syscall numbers. ptrace and the process_vm_* pair are the
	// interesting ones: Docker's default profile permits them.
	probe := syscallProbe + `
import errno
CALLS = [("ptrace", 117), ("process_vm_readv", 270), ("mount", 40),
         ("perf_event_open", 241), ("keyctl", 219), ("userfaultfd", 282)]
for name, nr in CALLS:
    rc, err = call(nr, 0, 0, 0, 0)
    print(f"{name}={errno.errorcode.get(err, err)}")
`
	req := ExecRequest{
		Language: "python", Code: probe,
		Limits: DefaultLimits(), Timeout: 20 * time.Second,
	}

	defOut, _, defRes := run(t, def, req)
	cusOut, _, cusRes := run(t, custom, req)
	if defRes.ExitCode != 0 || cusRes.ExitCode != 0 {
		t.Fatalf("probe failed (default exit %d, custom exit %d)", defRes.ExitCode, cusRes.ExitCode)
	}

	defMap, cusMap := parseProbe(defOut), parseProbe(cusOut)
	t.Logf("%-18s %-22s %s", "syscall", "docker default", "custom profile")
	var newlyBlocked int
	for _, name := range []string{"ptrace", "process_vm_readv", "mount",
		"perf_event_open", "keyctl", "userfaultfd"} {
		d, c := defMap[name], cusMap[name]
		note := ""
		// EPERM under our profile is the filter refusing the call. If the
		// default gave anything else, we closed something it left open.
		if c == "EPERM" && d != "EPERM" {
			note = "  <- newly blocked"
			newlyBlocked++
		}
		t.Logf("%-18s %-22s %s%s", name, d, c, note)
	}

	_, n, _ := SeccompProfileJSON()
	t.Logf("allowlist size: %d syscalls (deny-by-default)", n)
	t.Logf("syscalls this profile closes that Docker's default leaves open: %d", newlyBlocked)

	if newlyBlocked == 0 {
		t.Error("custom profile blocks nothing the default does not — it is not earning its complexity")
	}
}

func parseProbe(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, errno, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[name] = errno
		}
	}
	return m
}
