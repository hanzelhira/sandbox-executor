import { DEFAULT_LIMITS } from './Limits'
import type { LimitsState } from './Limits'

// Hostile payloads for the demo. Every one of these is covered by an
// integration test in internal/executor/docker_test.go — the dropdown shows
// the same containment the test suite asserts.
//
// These are textbook resource-exhaustion examples (the kind in any OS course),
// not exploits: they can only harm the container they run in, which is the
// entire point of showing them.

export interface Preset {
  id: string
  label: string
  /** What the payload attempts. */
  attempts: string
  /** The control that stops it, in kernel terms. */
  stoppedBy: string
  /** What the viewer should expect to see. */
  expect: string
  code: string
  /** Limit overrides that make the demo clearer. */
  limits?: Partial<LimitsState>
}

export const PRESETS: Preset[] = [
  {
    id: 'hello',
    label: 'Hello (harmless)',
    attempts: 'ordinary code, streamed live',
    stoppedBy: 'nothing — this one is meant to succeed',
    expect: 'exit 0',
    code: `import time

for i in range(5):
    print("tick", i)
    time.sleep(0.4)

print("done")
`,
  },
  {
    id: 'forkbomb',
    label: 'Fork bomb',
    attempts: 'spawn processes without limit',
    stoppedBy: 'cgroups pids.max',
    expect: 'fork() refused with EAGAIN',
    limits: { pids: 32, timeoutSec: 10 },
    code: `# Classic fork bomb: each child forks again, forever.
# pids.max caps the cgroup, so fork() starts failing.
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
  },
  {
    id: 'infinite',
    label: 'Infinite loop',
    attempts: 'never terminate',
    stoppedBy: 'deadline + SIGKILL of the whole container',
    expect: 'TIMED OUT',
    limits: { timeoutSec: 5 },
    code: `print("looping forever — the deadline will kill the container", flush=True)

while True:
    pass
`,
  },
  {
    id: 'memory',
    label: 'Memory bomb',
    attempts: 'allocate 2 GB',
    stoppedBy: 'cgroups memory.max → kernel OOM killer',
    expect: 'OOM-KILLED (note: stderr is empty)',
    limits: { memoryMB: 64 },
    code: `print("allocating 2 GB against a 64 MB cap...", flush=True)

x = bytearray(2 * 1024 * 1024 * 1024)

print("never reached")
`,
  },
  {
    id: 'network',
    label: 'Network exfiltration',
    attempts: 'phone home / leak data',
    stoppedBy: 'network namespace with no interface',
    expect: 'every connection fails',
    code: `# There is no network interface at all — not a firewall rule,
# so there is nowhere for packets to go.
import socket
socket.setdefaulttimeout(3)

for host, port in [("1.1.1.1", 53), ("example.com", 80)]:
    try:
        socket.create_connection((host, port))
        print("REACHABLE:", host)
    except Exception as e:
        print("blocked:", host, "->", type(e).__name__)
`,
  },
  {
    id: 'filesystem',
    label: 'Filesystem tampering',
    attempts: 'modify the system, persist between runs',
    stoppedBy: 'read-only rootfs; /tmp is noexec scratch',
    expect: 'writes refused except /tmp',
    code: `# Read-only rootfs: nothing survives, nothing can be tampered with.
for path in ("/evil", "/etc/passwd", "/usr/bin/evil", "/tmp/scratch"):
    try:
        open(path, "w").write("x")
        print(path, "WRITABLE")
    except Exception as e:
        print(path, "blocked:", type(e).__name__)
`,
  },
  {
    id: 'recursion',
    label: 'Runaway recursion (layered defence)',
    attempts: "blow the stack — after disabling Python's own guard",
    stoppedBy: "Python's limit first; the memory cap once that is removed",
    expect: 'OOM-KILLED — the kernel catches what Python no longer does',
    limits: { memoryMB: 64 },
    code: `# Run once as-is: Python's own RecursionError stops it politely.
# Then uncomment the setrecursionlimit line and run again: Python's
# guard is gone, and the sandbox catches it anyway.
import sys

# sys.setrecursionlimit(100_000_000)

def f():
    f()

f()
`,
  },
  {
    id: 'privileges',
    label: 'Privilege check',
    attempts: 'find out how much power it has',
    stoppedBy: 'non-root user, all capabilities dropped, no-new-privileges',
    expect: 'uid 1000, empty capability set',
    code: `import os

print("uid:", os.getuid(), " gid:", os.getgid())
print("euid:", os.geteuid(), "(0 would mean root)")

try:
    with open("/proc/self/status") as f:
        for line in f:
            if line.startswith(("CapEff", "NoNewPrivs")):
                print(line.strip())
except Exception as e:
    print("could not read status:", e)
`,
  },
]

// Presets start from the defaults and apply their own overrides, so switching
// between them never inherits a limit from the previously selected one.
export function limitsForPreset(p: Preset): LimitsState {
  return { ...DEFAULT_LIMITS, ...p.limits }
}
