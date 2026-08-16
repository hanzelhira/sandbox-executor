# Session-based sandboxes — plan

**Goal:** create a long-lived sandbox container, run code in it repeatedly with
state persisting between runs, and destroy it. This is the E2B / Modal / Jupyter
model, and the shape AI agents actually use.

**Keep the one-shot path.** `Execute` (create → exec once → destroy) stays as the
default and the safest demo. Sessions are added alongside it, not in place of it.

---

## The core mechanical change

Today: `python3 -c <code>` is PID 1. When it exits, the container exits. The run
*is* the container's life.

Session: a long-lived placeholder (`sleep infinity`, or a tiny supervisor) is
PID 1. Each run is a separate process started with **`docker exec`** inside the
already-running container. State (globals, files in /tmp) persists because the
container — and its writable tmpfs — lives on between runs.

Per run, startup drops from ~80 ms (container create) to just the exec cost
(~10-20 ms). Container creation happens once, at session creation.

---

## What we trade, and how to be honest about it

| Property | One-shot (now) | Session |
|---|---|---|
| Isolation between users/sessions | absolute | absolute (unchanged) |
| Isolation between runs | absolute | **traded for state — this is the point** |
| Startup per run | ~80 ms | ~15 ms |
| Cleanup | automatic per run | explicit destroy + idle TTL |
| Resource limits | per run | **per session** (cgroup wraps the container) |

The isolation panel must reflect this: within a session, "container destroyed per
run" becomes false, and the UI should say so rather than quietly claiming a
protection that no longer holds.

### Kill semantics get more subtle (the interview-worthy part)

- **Timeout** now kills the *exec'd process* (SIGKILL the exec PID), NOT the
  container — otherwise one infinite loop destroys the user's whole session and
  their state. One-shot timeout still kills the container.
- **OOM** is the kernel killing something in the cgroup. It may take out the
  placeholder PID 1 and thus the whole session. We detect this (session becomes
  unhealthy) and surface it — a session can *die* from an OOM, not just a run.
- **Fork bombs** accumulate PIDs against the session's shared `pids.max`. A fork
  bomb in a session can wedge the session (no free PIDs to exec the next run)
  even after the bomb's own run is killed. Correct behaviour: detect the session
  is unusable and mark it dead.
- Attack presets: keep running them **one-shot** by default (a fresh throwaway
  container), since several of them intentionally damage the container. Offer a
  "run in current session" toggle with a warning.

---

## Design

### Executor interface — additive

Keep `Execute` exactly as is. Add an optional session capability:

```go
// SessionExecutor is implemented by backends that support long-lived sandboxes.
// Executor stays the minimal one-shot contract; this is a separate interface so
// a backend can support one-shot without sessions.
type SessionExecutor interface {
    CreateSession(ctx context.Context, spec SessionSpec) (Session, error)
}

type Session interface {
    ID() string
    // Exec runs code in the live container, streaming output like Execute.
    Exec(ctx context.Context, code string, timeout time.Duration, out chan<- OutputChunk) (ExecResult, error)
    Healthy(ctx context.Context) bool   // false after OOM/fork-bomb wedge
    Destroy(ctx context.Context) error
}

type SessionSpec struct {
    Language string
    Limits   ResourceLimits   // applied to the container, i.e. per session
}
```

`DockerExecutor` implements `SessionExecutor`. Container is created with the same
lockdown as one-shot (network none, read-only rootfs, tmpfs, caps dropped,
seccomp, non-root) but with `sleep infinity` as the command and `Tty: false`.
Each `Exec` uses the Docker exec API with the same demux/streaming machinery we
already have in `Execute` — factor the stdcopy/chunkWriter part out so both share
it.

Startup metric for a session run = time from exec-create to first byte, recorded
separately so the session speedup is visible against one-shot.

### Registry / lifecycle — the real work

- `SessionRegistry`: id → live `Session`, plus createdAt / lastUsedAt.
- **Idle TTL reaper**: a goroutine destroys sessions idle > N minutes. Without
  this, sessions leak containers forever — the exact thing `TestNoContainersLeak`
  forbids.
- **Concurrency cap**: max live sessions (generalise the existing semaphore).
  Creating past the cap → 429, same as runs.
- **Explicit destroy** on `DELETE`, and destroy-all on server shutdown.
- Health: after each exec, if the session is unhealthy, mark it dead and stop
  routing to it.

### API — new session verbs (one-shot routes unchanged)

```
POST   /sessions              {language, limits} -> {sessionId}
POST   /sessions/{id}/exec    {code, timeoutMs}  -> {executionId}   (async)
WS     /stream/{id}           (reused as-is — exec streams like a run)
GET    /sessions              -> [{id, createdAt, lastUsedAt, healthy, runs}]
DELETE /sessions/{id}         -> 204
```

`/config` gains a note that sessions relax per-run isolation.

### UI — the menu

Left rail listing sessions: **+ New sandbox**, each row selectable, an ✕ to
destroy, a health dot. A "scratch (one-shot)" pseudo-entry at the top preserves
today's behaviour and stays selected by default.

- Selecting a session routes Run through `/sessions/{id}/exec`.
- One-shot selected → today's `/execute`.
- A demo that sells it: run `x = 41` in a session, then `print(x + 1)` in the
  same session → `42`. State persistence, visible in two clicks. This is the
  session equivalent of the attack-preset "wow".

---

## Testing (must hold before it ships)

- **State persists across execs** in one session; does NOT leak across sessions.
- **Timeout kills the run, session survives** — infinite loop in a session, then
  a normal run in the same session still works.
- **Session isolation** — network none, read-only rootfs etc. still hold inside
  an exec (re-run the containment suite against a session).
- **No leaks**: create N sessions, destroy them, container count returns to
  baseline; idle reaper destroys an abandoned session.
- **Cap enforced**: past max sessions → 429.
- **Fork bomb wedges only its own session**, detected as unhealthy, others fine.

---

## Build order

1. Factor shared streaming (stdcopy → chunks) out of `Execute` so exec reuses it.
2. `SessionExecutor` + `DockerExecutor.CreateSession` + `dockerSession.Exec`
   using the exec API. Unit-test state persistence and run-timeout-survives.
3. `SessionRegistry` + idle reaper + cap + destroy-all-on-shutdown.
4. API routes; reuse `/stream`.
5. Re-run containment suite against a session.
6. UI: session rail, routing, the 41→42 demo.
7. Update `/config` + isolation panel wording; CLAUDE.md.

## Definition of done

Create a sandbox, run `x = 41`, run `print(x+1)` → `42`. Infinite loop in a
session is killed and the session still works after. Destroy it and no container
leaks. One-shot path and attack presets unchanged.
