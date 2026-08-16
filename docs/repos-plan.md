# Repo-backed sandboxes — plan

**Goal:** load a whole repository into a sandbox — by uploading a folder/zip or
by giving a git URL — then run real commands against it (`pip install`, `pytest`,
`python main.py`), not just a single `python -c` snippet. The single-file editor
becomes a file tree + editor + command terminal over the loaded repo.

**This is the biggest change to the threat model so far.** Read the network
section before anything else — it is the decision the whole feature turns on.

---

## The core insight: a repo sandbox IS a session

We already built long-lived sessions (`docs/sessions-plan.md`). A repo sandbox is
exactly that, plus a populated filesystem:

1. Create a session container (persistent, locked down).
2. Populate `/tmp/work` with the repo's files.
3. Exec commands in `/tmp/work` repeatedly, state persisting.
4. Destroy when done.

So most of the lifecycle, cap, reaper, and teardown machinery already exists. The
new parts are **getting files in**, **running arbitrary commands**, and **a file-
tree UI**.

---

## The network dilemma (decide this first)

Our headline isolation control is `network=none` — the sandbox has no interface,
so untrusted code cannot exfiltrate or phone home. Repos break this in two places:

- **git clone** needs network to reach GitHub.
- **dependency install** (`pip install`, `npm install`) needs network to reach
  PyPI/npm.

You cannot both "run an arbitrary repo with its deps" and "have zero network"
inside one container. The honest options:

### Option A — Ingest on the control plane, run with no network (recommended)
The **server** (which has network) does the clone and, optionally, the dependency
install into a directory; that directory is then copied into a `network=none`
sandbox that only *runs* the code. The runner stays airtight — the exact
build-vs-run split from the compiled-languages discussion.

- Clone: `git clone` runs in a restricted helper (own container, network, but
  never runs repo code — it only fetches files), depth-1, size-capped.
- Deps: run `pip install` in a **build** container that has network but is still
  cap-dropped/non-root, into a target dir. Then copy that dir into the runner.
- Run: the locked-down `network=none` session, populated, executes commands.

Pro: the code that actually runs never touches the network. Con: two container
phases, more moving parts, and "install" happens in a less-isolated context (but
one that never runs arbitrary entrypoints — only the package manager).

### Option B — Egress allowlist during a build phase
Give the build/install phase network egress **only to PyPI/npm** via an HTTP proxy
that allowlists those hosts; run phase still `network=none`. More faithful than
open network, more work (a proxy), and allowlists leak (PyPI serves arbitrary
package code).

### Option C — No deps, no clone-in-sandbox; upload only, offline only
Upload a repo, run only what the base image already provides (stdlib Python). No
`pip install`, no git. Simplest and keeps `network=none` everywhere, but many real
repos won't run. Good honest MVP.

**Recommendation:** ship **C first** (upload + run, fully offline, zero threat-
model change), then add **A** as the "clone & install" upgrade behind an explicit,
clearly-labelled toggle. Never quietly give the run container network — that would
silently void the property the whole project advertises.

---

## Getting files in

The session container has a writable tmpfs at `/tmp`. Populate `/tmp/work` **after
the container is started** (tmpfs is already mounted, so files are not masked):

- `cli.CopyToContainer(ctx, id, "/tmp", tarStream)` — Docker's tar-upload API.
  Build the tar from the uploaded files or the cloned checkout.
- Enforce on the way in: **max total bytes**, **max file count**, **reject `..`
  and absolute paths** (path traversal), **reject symlinks** that escape the root,
  and cap per-file size (zip-bomb / tarbomb guard). This validation is the highest-
  risk code in the feature — treat it like the auth boundary.
- tmpfs is `noexec`, which still allows interpreted code (`python foo.py` execs the
  interpreter, not `foo.py`). Compiled binaries in the repo won't run — acceptable,
  and worth surfacing in the UI.

Ingestion sources:
- **Zip upload** — browser sends a `.zip`; server unpacks (with the guards above)
  and tars into the container.
- **Folder upload** — `<input webkitdirectory>` sends many files; same guards.
- **Git URL** (Option A) — server clones depth-1 to a temp dir, tars it in. Guard
  the URL: only `https://`, only allowlisted hosts if you want to be strict, a size
  cap, and a clone timeout. No credentials, so private repos are out of scope.

---

## Running commands

Today: `python3 -u -c <code>`. For repos we need arbitrary commands in a working
dir. Extend the session exec:

```
POST /sessions/{id}/exec   {argv: ["pytest","-q"], cwd: "/tmp/work"}   (preferred)
```

- Prefer an **argv array** over a shell string — no shell = no shell injection, the
  same structural choice we made for the single-file path. Offer common presets
  ("run tests", "run main.py") that fill argv.
- Everything else (streaming, timeout kills the exec not the container, output demux,
  the WebSocket) is reused unchanged.
- `WorkingDir` = the repo root so relative imports and paths work.

Resource limits still bound the whole session container; a repo that forks or
allocates hits the same cgroup walls. The attack presets stay one-shot.

---

## UI redesign (replaces the single editor)

The left editor becomes a small IDE over the loaded repo:

- **Load bar**: "Upload folder", "Upload .zip", and (Option A) a git-URL field.
- **File tree**: the repo's files; click to open in Monaco (multi-model — one model
  per file, which Monaco supports natively).
- **Editor**: edits the selected file; edits are pushed back into the container
  (`CopyToContainer` of the single changed file) so a re-run sees them.
- **Command bar / terminal**: run argv against the repo; output in the existing
  xterm terminal. Presets for install/test/run.
- The isolation panel gains a line noting the repo source and whether network was
  used to fetch/install (honesty in the UI).

"scratch" (one-shot single snippet) can stay as a lightweight mode, or be dropped —
your call. The repo sandbox subsumes it.

---

## Security checklist (must all hold)

- Upload/clone: total-size cap, file-count cap, per-file cap, `..`/absolute-path
  rejection, symlink-escape rejection, clone timeout, `https`-only URLs.
- Run container remains `network=none`, read-only rootfs, caps dropped, non-root,
  seccomp, per-session limits — unchanged.
- If Option A/B: the phase that has network **never runs the repo's entrypoint** —
  it only clones or invokes the package manager. Document this boundary loudly.
- Idle reaper + destroy still reclaim repo sandboxes (they hold more disk via
  tmpfs, so consider a shorter TTL or a per-session disk cap).

---

## Build order

1. **Filesystem population**: `CopyToContainer` into a session's `/tmp/work`, with
   the tar-build + validation guards. Unit-test traversal/zip-bomb rejection.
2. **argv exec**: extend session exec to take argv + cwd; keep `-c` path for scratch.
3. **Offline MVP (Option C)**: zip/folder upload → populated session → run presets.
   Fully offline, no threat-model change. Ship this.
4. **File-tree UI** + Monaco multi-model + push-edits-back.
5. **Option A (clone & install)** behind a labelled toggle: git-fetch helper and a
   networked build phase, copying results into the airtight runner.
6. Update `/config`, isolation panel wording, CLAUDE.md, threat model.

## Definition of done (MVP, Option C)

Upload a small Python repo (e.g. a package with a test suite), see its file tree,
open a file, click "run tests", watch pytest stream in the terminal — all inside a
`network=none`, read-only, non-root session that leaks nothing and is destroyed
cleanly. Clone + pip install is a labelled, opt-in follow-up that never gives the
run container network.
