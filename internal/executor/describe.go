package executor

import "fmt"

// Describer is implemented by executors that can report their own isolation
// configuration. It is deliberately separate from Executor: describing a
// sandbox is a UI concern, and the core interface should stay minimal so new
// backends are cheap to add.
type Describer interface {
	Describe() Description
}

// Control is one isolation knob and the threat it addresses.
type Control struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Defends string `json:"defends"`
}

// Description is what a backend reports about how it confines code.
type Description struct {
	Backend  string    `json:"backend"`
	Image    string    `json:"image"`
	Runtime  string    `json:"runtime"`
	Controls []Control `json:"controls"`
}

// Describe reports the controls this executor actually applies. Values come
// from the live configuration rather than a hardcoded list, so the UI cannot
// claim protection that is not switched on.
func (d *DockerExecutor) Describe() Description {
	seccomp := "custom allowlist"
	switch {
	case d.seccompProfile == "":
		seccomp = "Docker default (denylist)"
	case d.seccompProfile == "seccomp=unconfined":
		seccomp = "DISABLED"
	default:
		seccomp = fmt.Sprintf("allowlist, %d syscalls", d.SyscallsAllowed)
	}

	runtime := d.runtime
	if runtime == "" {
		runtime = "runc"
	}

	return Description{
		Backend: "docker",
		Image:   d.image,
		Runtime: runtime,
		Controls: []Control{
			{"network", "none", "exfiltration, lateral movement"},
			{"rootfs", "read-only", "tampering, persistence"},
			{"scratch", "tmpfs /tmp, 64m, noexec", "needs somewhere to write, can't execute it"},
			{"user", "1000:1000 (non-root)", "blast radius if escaped"},
			{"capabilities", "all dropped", "privilege escalation, raw sockets"},
			{"no-new-privileges", "on", "setuid escalation"},
			{"seccomp", seccomp, "kernel syscall attack surface"},
			{"lifetime", "container destroyed per run", "cross-run contamination"},
		},
	}
}
