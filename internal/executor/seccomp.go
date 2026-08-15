package executor

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Custom seccomp profile.
//
// Docker's default profile is a DENYLIST: it permits most of the ~350 Linux
// syscalls and blocks roughly 40 known-dangerous ones. This profile inverts
// that to an ALLOWLIST — everything is denied unless CPython demonstrably
// needs it — so a kernel bug in an unlisted syscall is unreachable rather
// than merely unblocked-by-omission.
//
// The base list is empirical: hack/profile-syscalls.sh straces representative
// workloads (interpreter startup, imports, /tmp file IO, threads, fork,
// subprocess, exceptions, large allocations, sleep) and prints the union of
// observed syscalls. The remaining groups cover calls that profiling cannot
// see or that only appear on paths the workloads did not take — documented
// individually below, because "add whatever makes it work" is how allowlists
// silently rot back into denylists.

// observed on aarch64 by hack/profile-syscalls.sh.
var seccompObserved = []string{
	"brk", "clock_nanosleep", "clone", "clone3", "close", "close_range",
	"dup3", "epoll_create1", "execve", "faccessat", "fcntl", "fstat",
	"futex", "getcwd", "getdents64", "getegid", "geteuid", "getgid",
	"getrandom", "gettid", "getuid", "ioctl", "lseek", "madvise", "mmap",
	"mprotect", "mremap", "munmap", "newfstatat", "openat", "pipe2",
	"ppoll", "prlimit64", "read", "readlinkat", "renameat", "rseq",
	"rt_sigaction", "rt_sigprocmask", "set_robust_list", "set_tid_address",
	"wait4", "write",
}

// Required but not necessarily observed.
//
// vDSO calls (clock_gettime, gettimeofday, getcpu, time) usually resolve in
// userspace and so never appear under strace, but they fall back to real
// syscalls on some kernels/arches — omitting them causes rare, baffling
// hangs. exit/exit_group are mandatory for any process to terminate.
// Signal-return and stack calls are needed for exception and fault handling.
var seccompRuntime = []string{
	"exit", "exit_group",
	"rt_sigreturn", "sigaltstack", "rt_sigtimedwait", "rt_sigsuspend",
	"clock_gettime", "clock_getres", "gettimeofday", "time", "getcpu",
	"nanosleep", "sched_yield", "sched_getaffinity", "sched_getparam",
	"sched_getscheduler", "sched_get_priority_max", "sched_get_priority_min",
	"getpid", "getppid", "gettid", "getrusage", "times", "uname", "sysinfo",
	"membarrier", "restart_syscall", "tgkill", "tkill", "kill",
	"set_tls", "arch_prctl", "prctl",
}

// File and directory operations Python performs on paths our workloads did
// not exercise (tempfile, shutil, pathlib, logging).
var seccompFileIO = []string{
	"openat2", "statx", "stat", "lstat", "fstatat64", "access",
	"pread64", "pwrite64", "readv", "writev", "preadv", "pwritev",
	"lseek64", "ftruncate", "truncate", "fsync", "fdatasync",
	"unlink", "unlinkat", "mkdir", "mkdirat", "rmdir", "rename", "renameat2",
	"link", "linkat", "symlink", "symlinkat", "readlink",
	"chmod", "fchmod", "fchmodat", "chdir", "fchdir", "getdents",
	"dup", "dup2", "pipe", "poll", "select", "pselect6", "epoll_create",
	"epoll_ctl", "epoll_wait", "epoll_pwait", "eventfd", "eventfd2",
	"fadvise64", "flock", "umask", "utimensat",
}

// Container-init requirements. runc applies the seccomp filter and only then
// finishes setting up the process, so anything it still needs must be
// allowed or the container dies before our code runs — "unable to get
// capability version from the kernel" is what a missing capget looks like.
//
// Allowing capset is not a privilege hole here: the process starts with an
// empty capability set (CapDrop ALL) and carries no-new-privileges, so it can
// only ever drop what it does not have.
// Allowing the setuid/setgid family looks alarming and is not: seccomp and
// capabilities are independent gates. runc calls them while still privileged,
// to switch to uid 1000. Our Python process runs afterwards with an empty
// capability set and no-new-privileges, so the kernel refuses the same calls
// on capability grounds regardless of what the filter permits — verified by
// TestSeccompUserSwitchStillBlocked.
var seccompContainerInit = []string{
	"capget", "capset", "setgroups", "setgroups32",
	"setuid", "setgid", "setreuid", "setregid", "setresuid", "setresgid",
	"setuid32", "setgid32", "setresuid32", "setresgid32",
	// runc stats /proc while finishing user setup; without these it fails
	// with "unable to setup user: fstatfs ...: operation not permitted".
	"fstatfs", "statfs", "fstatfs64", "statfs64",
}

// Memory and process management beyond the observed set. fork/vfork appear on
// x86_64 where clone is not always used; execveat backs some subprocess paths.
var seccompProcess = []string{
	"fork", "vfork", "execveat", "waitid", "wait4",
	"mlock", "munlock", "mlockall", "munlockall", "msync", "mbind",
	"set_mempolicy", "get_mempolicy", "process_mrelease",
	"setsid", "getpgrp", "getpgid", "getsid",
	"getresuid", "getresgid", "getgroups",
	"futex_waitv", "get_robust_list",
}

// AF_UNIX sockets only in practice — there is no network namespace to reach,
// but CPython's multiprocessing and socketpair paths still call these, and a
// clean ENOSYS-style failure is worse to debug than a working socketpair on a
// namespace with no interfaces.
var seccompLocalSockets = []string{
	"socket", "socketpair", "bind", "listen", "accept", "accept4",
	"connect", "getsockname", "getpeername", "sendto", "recvfrom",
	"sendmsg", "recvmsg", "shutdown", "setsockopt", "getsockopt",
}

// Deliberately NOT allowed, for the record — these are the reason an
// allowlist beats a denylist. Docker's default profile permits several of
// them (notably ptrace and the process_vm_* pair).
//
//	mount, umount2, pivot_root, chroot, setns, unshare  — namespace escape
//	ptrace, process_vm_readv, process_vm_writev         — process injection
//	bpf, perf_event_open, kexec_load, init_module       — kernel surface
//	delete_module, finit_module, reboot, swapon/swapoff
//	keyctl, add_key, request_key                        — kernel keyring
//	userfaultfd                                         — exploit primitive
//	io_uring_setup/enter/register                       — large attack surface
//	clock_settime, settimeofday, sethostname, acct      — host state
//	quotactl, nfsservctl, personality, modify_ldt
var seccompDenied = []string{
	"mount", "umount2", "pivot_root", "chroot", "setns", "unshare",
	"ptrace", "process_vm_readv", "process_vm_writev",
	"bpf", "perf_event_open", "kexec_load", "init_module", "finit_module",
	"delete_module", "reboot", "swapon", "swapoff",
	"keyctl", "add_key", "request_key", "userfaultfd",
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	"clock_settime", "settimeofday", "sethostname", "setdomainname", "acct",
	"quotactl", "personality", "modify_ldt", "iopl", "ioperm",
	"mknod", "mknodat",
}

type seccompSyscall struct {
	Names  []string `json:"names"`
	Action string   `json:"action"`
}

type seccompProfile struct {
	DefaultAction string           `json:"defaultAction"`
	Architectures []string         `json:"architectures"`
	Syscalls      []seccompSyscall `json:"syscalls"`
}

var (
	seccompOnce sync.Once
	seccompJSON string
	seccompErr  error
	seccompN    int
)

// AllowedSyscalls returns the deduplicated allowlist.
func AllowedSyscalls() []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{
		seccompObserved, seccompRuntime, seccompContainerInit,
		seccompFileIO, seccompProcess, seccompLocalSockets,
	} {
		for _, name := range group {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// DeniedSyscalls returns the syscalls this profile explicitly refuses. They
// are absent from the allowlist anyway; the list exists so the denial is
// documented and testable rather than incidental.
func DeniedSyscalls() []string { return seccompDenied }

// SeccompProfileJSON returns the profile in Docker's seccomp format, ready to
// pass as SecurityOpt "seccomp=<json>".
func SeccompProfileJSON() (string, int, error) {
	seccompOnce.Do(func() {
		allowed := AllowedSyscalls()
		p := seccompProfile{
			// ERRNO rather than KILL_PROCESS: a refused syscall surfaces as a
			// normal EPERM the runtime can report, instead of a silent death
			// that looks identical to an OOM kill in the results.
			DefaultAction: "SCMP_ACT_ERRNO",
			// Both arches so the same profile works on an x86_64 host.
			Architectures: []string{
				"SCMP_ARCH_AARCH64",
				"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32",
			},
			Syscalls: []seccompSyscall{{Names: allowed, Action: "SCMP_ACT_ALLOW"}},
		}
		b, err := json.Marshal(p)
		if err != nil {
			seccompErr = fmt.Errorf("marshal seccomp profile: %w", err)
			return
		}
		seccompJSON, seccompN = string(b), len(allowed)
	})
	return seccompJSON, seccompN, seccompErr
}
