# sandbox-executor

Run untrusted Python safely, and watch it fail when it misbehaves.

A service that executes untrusted code inside a resource-capped, network-isolated,
seccomp-filtered Docker container and streams the output live to a browser IDE.
It's the machinery underneath things like online judges, notebook backends, and
the code-execution tools AI agents use — turning "running a stranger's code" from
a reckless act into a measured, bounded, observable one.

> **Isolation boundary:** at this stage the sandbox is a hardened, shared-kernel
> container (seccomp + cgroups + namespaces), **not** a VM. A kernel exploit
> escapes any container; that's the definitional limit, and the reason gVisor is
> the planned next step (`docs/gvisor-plan.md`). This README never claims more
> isolation than the code provides.

<!-- TODO: demo GIF of the attack presets being caught -->

---

## What it does

Three ways to run code, all behind one `Executor` interface:

- **One-shot** — a fresh locked-down container per run, destroyed on exit. The
  default, and the safest demo. Includes a **"try an attack" dropdown** (fork
  bomb, infinite loop, 2 GB alloc, network call, filesystem tampering, runaway
  recursion, privilege check) so you can watch each limit catch the attack live.
- **Sessions** — a long-lived sandbox you run code in repeatedly, with state
  persisting between runs. Per-run startup drops from a cold-container ~80 ms to a
  `docker exec` of a few ms.
- **Repos** — upload a folder or `.zip` into a session and run commands against it
  (`python main.py`, `unittest`, `ls`) from a file-tree + editor IDE. Fully
  offline; the files never leave your machine.

Every run reports how it ended — not just "failed", but **which** limit fired:
`TIMED OUT`, `OOM-KILLED`, a real exit code, or a traceback.

---

## Isolation model

Every container — one-shot or session — is created with all of:

| Control | Setting | Defends against |
|---|---|---|
| Memory cap | `memory.max` (+ swap = memory) | exhaustion; triggers the kernel OOM killer |
| CPU cap | `cpu.max` | CPU hogging / mining |
| PID cap | `pids.max` | fork bombs (`fork()` returns `EAGAIN`) |
| No network | `NetworkMode: none` | exfiltration, lateral movement |
| Read-only root | `ReadonlyRootfs` | tampering, persistence |
| Scratch only | tmpfs `/tmp`, 64m, `noexec` | needs a writable spot, can't execute from it |
| Drop caps | `CapDrop: ALL` | privilege escalation, raw sockets |
| Non-root | uid 1000 | blast radius |
| No new privs | `no-new-privileges` | setuid escalation |
| **Custom seccomp** | **182-syscall allowlist, deny-by-default** | shrinks the kernel attack surface |
| Time limit | context deadline → SIGKILL | infinite loops |
| Ephemeral | container destroyed per run | cross-run contamination (one-shot) |

**Enforcement is the kernel's**, not Docker's — Docker just translates this config
into namespaces (what the process can *see*), cgroups (what it can *use*), and
seccomp/capabilities (what it can *ask the kernel to do*).

The seccomp profile is an **allowlist built from evidence**: representative
workloads are straced (`hack/profile-syscalls.sh`) and only the observed syscalls
(plus documented runtime/init needs) are permitted — everything else returns
`EPERM`. Measured against Docker's default profile, `ptrace` **succeeds** under
the default and is blocked here; the profile permits 182 syscalls versus ~300.

### What this does and doesn't stop

**Stops:** misconfiguration-class escapes (the opposite of `--privileged`; no
docker.sock, no bind mounts, non-root, no caps), resource exhaustion,
exfiltration, and persistence between runs.

**Does not stop:** a working Linux **kernel exploit** — the kernel is shared, and
a syscall bug escapes any container. Disk I/O is uncapped. Side-channel
(Spectre-class) attacks are out of scope. These are the reasons the roadmap moves
toward gVisor (a user-space kernel).

---

## Verified behavior

All of these are covered by integration tests against real containers
(`internal/executor/*_test.go`):

| Attack | Result |
|---|---|
| Infinite loop | `TimedOut`, whole container SIGKILLed at the deadline |
| 2 GiB alloc vs 64 MiB cap | `OOMKilled`, exit 137 (128 + SIGKILL) |
| Fork bomb vs `pids.max` | every child gets `EAGAIN`; host stays healthy |
| Network egress | fails — no interface exists |
| Read-only rootfs | `/` and `/etc` refused; `/tmp` writable |
| Non-root | uid 1000, `CapEff: 0000000000000000` (read from `/proc/self/status`) |
| Repo run | cross-file imports resolve; still non-root, network-none, read-only |
| Sessions | state persists across runs; isolated between sessions; timeout kills the run, not the session |
| No leaks | containers destroyed on exit, timeout, and server shutdown |

### Measured (Apple M3 Pro, Docker Desktop, aarch64 — laptop numbers)

- One-shot cold-container startup: **p50 ~70–90 ms, p99 ~100–160 ms**
- Session/repo exec (container already warm): **~2–6 ms**
- Custom seccomp overhead: **~+11 ms** startup vs Docker's default

Numbers are from a laptop under a Linux VM; absolute values differ on bare metal,
but the relative comparisons hold since everything is measured on the same host.

---

## Run it

Needs Go, Node, and a running Docker daemon (Docker Desktop, OrbStack, or Colima).

```bash
docker pull python:3.12-slim
make run        # builds one binary with the UI embedded, serves on :8080
```

Then open <http://localhost:8080>.

For frontend hot-reload during development, run both servers together (Ctrl-C
stops both):

```bash
make dev        # API on :8080, Vite UI on :5173
```

`make test` runs everything (needs Docker); `make test-short` skips the
container integration tests.

Auth is a static bearer token (`SANDBOX_TOKEN`, default `devtoken` in dev) —
adequate for a local demo, not a real auth system, by design.

---

## API

| Route | Purpose |
|---|---|
| `POST /execute` | one-shot run → `{executionId}` |
| `POST /execute/sync` | one-shot run, response in one shot |
| `WS /stream/{id}` | live output frames, then a final result frame |
| `POST /sessions` | create a persistent sandbox → `{sessionId}` |
| `POST /sessions/{id}/exec` | run `code` or `argv` in a session |
| `POST /sessions/{id}/files` | upload a repo (zip or JSON+base64) |
| `GET /sessions/{id}/files` | list the tree, or read one file with `?path=` |
| `DELETE /sessions/{id}` | destroy a sandbox |
| `GET /stats` | p50/p99 latency + live capacity |
| `GET /config` | the isolation controls actually applied |

Over-capacity requests get `429` with `Retry-After` — a bounded queue that
refuses fast rather than growing without limit.

---

## Design notes

- **The `Executor` / `SessionExecutor` interfaces are the seam.** The API and UI
  never touch Docker-specific types, so a future `GvisorExecutor` is a new file,
  not a rewrite. This has already survived several scope changes untouched.
- **No shell, anywhere.** Code and commands are passed as argv straight to
  `execve`, so shell injection is structurally impossible rather than filtered.
- **Kill semantics are deliberate.** A one-shot timeout kills the container; a
  session timeout kills only the exec'd process, so a bad run doesn't destroy the
  sandbox's state. OOM and fork-bomb wedges are detected and the session is
  retired.
- **Upload validation is treated like auth** — path-traversal, size, count, and
  zip-bomb guards, table-tested against malicious inputs.

---

## Scope

Deliberately **local-only**: no rented servers, no public deploy (running
arbitrary code on a public endpoint is exactly what this project's own threat
model argues against). Python only. In-memory registry, no database.

**Planned:** gVisor as a second backend, with a measured Docker-vs-gVisor
comparison (`docs/gvisor-plan.md`). **Scaffolded:** git-clone repo sourcing
(`docs/repos-plan.md`, Option A) — disabled because cloning needs network, which
would run on the control plane and never inside the isolated runner.

The attack payloads in the demo are textbook resource-exhaustion examples that can
only affect the container they run in — the whole point is watching containment
work.
