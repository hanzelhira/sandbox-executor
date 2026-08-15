# gVisor backend — plan

**Goal:** a second `Executor` implementation running on gVisor, and a measured
comparison against the Docker backend. The comparison *is* the deliverable —
"I added gVisor" is worth much less than "here is what it costs and what it buys."

**Status:** not started. Blocked on an environment decision (step 0).

---

## Why

Today's honest ceiling: the container shares the host kernel. Every hardened knob
in `docker.go` reduces what untrusted code can *reach*, but a memory-corruption
bug in any reachable syscall handler is an escape, because the kernel is the thing
doing the containing. The custom seccomp profile shrank that surface to 182
syscalls; it did not change its nature.

gVisor changes the nature. Syscalls hit the **Sentry**, a user-space kernel
written in Go running unprivileged, which implements the Linux ABI itself and
forwards only a small vetted set to the real kernel. An escape needs a bug in the
Sentry, *then* an escape from the Sentry's own seccomp jail, *then* a bug in the
narrow set it actually uses.

---

## Step 0 — environment (do this first, it is the only real risk)

`runsc` must be installed **inside the Linux VM** and registered as an alternate
Docker runtime. Docker Desktop's VM is an appliance: `daemon.json` is editable via
its GUI, but placing a binary on a persistent path inside it is the hard part.

**Plan: switch to Colima**, which is a normal VM you can `ssh` into.

```bash
brew install colima docker
colima start --cpu 4 --memory 6 --disk 30
# inside the VM:
colima ssh
  curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  # ...add repo, then:
  sudo apt-get update && sudo apt-get install -y runsc
  sudo runsc install            # writes the runtime into /etc/docker/daemon.json
  sudo systemctl restart docker
```

Then `docker info` should list `runsc` under Runtimes, and this must print `hi`:

```bash
docker run --rm --runtime=runsc python:3.12-slim python3 -c 'print("hi")'
```

**Nothing in the Go code changes** — same socket, same API. Budget an hour.

### Risks, honestly

- **arm64 + nested VM.** gVisor supports arm64 and the default `systrap` platform
  needs no KVM, so this should work on Apple Silicon inside Colima's VM — but it
  is less trodden than x86 and unverified here. If `systrap` misbehaves, try
  `--platform=ptrace` (slower, more portable).
- **Timebox it.** If `runsc` is not running Python inside two hours, stop and take
  the fallback below rather than sinking a day into it.
- **Fallback if blocked:** make the seccomp work the depth story instead — measure
  the syscall surface reduction properly, add an audit-mode profile
  (`SCMP_ACT_LOG`) that records what real workloads attempt, and write that up.
  That is still a genuine isolation-tradeoff piece with numbers.

---

## Step 1 — `GvisorExecutor`

Mostly `DockerExecutor` with `HostConfig.Runtime = "runsc"`. Rather than copy the
file, factor the shared container lifecycle and take the runtime name as a field:

```go
type DockerExecutor struct {
    cli     *client.Client
    image   string
    runtime string // "" = default runc, "runsc" = gVisor
    ...
}

func NewGvisorExecutor(image string) (*DockerExecutor, error)
```

Then a constructor per backend, one implementation. Watch for:

- **Seccomp interaction.** Our custom profile is written against the *Linux* ABI.
  gVisor implements a subset of Linux and applies its own filtering, so the two
  layer awkwardly. Start with `SeccompDockerDefault` under runsc, confirm it runs,
  then try the custom profile and record whether it still works. This is a finding
  either way and belongs in the writeup.
- **OOM detection.** `State.OOMKilled` comes from the host cgroup. Verify it is
  still populated under runsc — if not, memory kills need a different signal, and
  the "distinguish OOM from timeout" behaviour must be preserved.
- **PID limits.** `pids.max` is a host cgroup control; the Sentry manages processes
  internally. Confirm the fork bomb is still contained and by what.

**Every existing test in `docker_test.go` must pass unchanged against the new
backend.** Parameterise the suite over backends rather than duplicating it — if a
containment property does not hold under gVisor, that is exactly what the tests
should tell us.

## Step 2 — wire it through

- `SANDBOX_BACKEND=docker|gvisor` in `cmd/server`.
- Report the backend in `ExecResult` (or alongside it) so the UI can display it.
- A backend selector in the UI, so the demo can switch live and show the same
  attack being caught by a different isolation boundary.

## Step 3 — the measurement (the actual deliverable)

Extend `TestLatencyProfile` to run both backends and print a table:

| | Docker (runc) | gVisor (runsc) |
|---|---|---|
| startup p50 / p99 | 81 / 113 ms | ? |
| total p50 / p99 | 99 / 128 ms | ? |
| CPU-bound work (compute loop) | baseline | ? |
| syscall-heavy work (file IO loop) | baseline | ? |

The last two rows matter: gVisor's cost is concentrated in syscalls, so a
CPU-bound workload should be near-native while an IO-heavy one should be
visibly slower. Showing that split is the difference between reporting a number
and understanding it.

Also worth capturing: does `ptrace` behave differently? What does the Sentry
report for the attack presets?

## Step 4 — write it up

A short `docs/isolation-backends.md`: the three boundaries (default container,
hardened container, gVisor), what each stops, what each costs, and when the
latency is worth paying. Reuse the threat-model section from CLAUDE.md.

Honest framing to keep: gVisor is stronger than a container and weaker than a
VM. Never claim hardware isolation — that would need a hypervisor, which is out
of scope for this project.

---

## Definition of done

- `docker run --runtime=runsc` works locally.
- `GvisorExecutor` passes the entire existing containment suite, or the failures
  are documented and explained.
- One command produces the comparison table.
- The UI can switch backends and catch the same attack under both.
