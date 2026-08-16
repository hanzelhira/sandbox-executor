import { useCallback, useEffect, useRef, useState } from 'react'
import {
  startRun,
  startArgv,
  streamExecution,
  readRepoFile,
  writeRepoFile,
  deleteRepoFile,
  listRepoFiles,
  createSession,
} from './api'
import type { ExecResult } from './api'
import { TerminalPane } from './Terminal'
import type { TerminalHandle } from './Terminal'
import { CodeEditor } from './Editor'
import { LimitsPanel, DEFAULT_LIMITS, toApiLimits } from './Limits'
import type { LimitsState } from './Limits'
import { PRESETS, limitsForPreset } from './presets'
import type { Preset } from './presets'
import { IsolationPanel, StatsBar, SectionLabel } from './Isolation'
import { SessionRail } from './Sessions'
import { RepoPanel } from './Repo'
import { FileExplorer } from './FileExplorer'

export default function App() {
  const [preset, setPreset] = useState<Preset>(PRESETS[0])
  const [code, setCode] = useState(PRESETS[0].code)
  const [limits, setLimits] = useState<LimitsState>(DEFAULT_LIMITS)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [running, setRunning] = useState(false)
  const [runCount, setRunCount] = useState(0)
  // null = one-shot scratch; a string = run inside that persistent sandbox.
  const [sessionId, setSessionId] = useState<string | null>(null)
  // When a repo file is open, the editor edits that file instead of a snippet.
  const [repoFile, setRepoFile] = useState<string | null>(null)
  const [repoFiles, setRepoFiles] = useState<string[]>([])
  const term = useRef<TerminalHandle | null>(null)
  // Kept in a ref so the keyboard shortcut always calls the current closure
  // without re-binding the listener on every state change.
  const runRef = useRef<() => void>(() => {})

  // Live mirrors of the per-sandbox editor state. selectSession snapshots the
  // OUTGOING sandbox at click time, but React state is async — reading `code`
  // (which Monaco drives at high frequency) from the render closure can save a
  // stale value into the wrong sandbox. These refs are always current, so the
  // snapshot is correct even across rapid switches.
  const codeRef = useRef('')
  const repoFileRef = useRef<string | null>(null)
  const resultRef = useRef<ExecResult | null>(null)
  const sessionIdRef = useRef<string | null>(null)

  function updateCode(v: string) {
    codeRef.current = v
    setCode(v)
  }
  function updateRepoFile(v: string | null) {
    repoFileRef.current = v
    setRepoFile(v)
  }
  function updateResult(v: ExecResult | null) {
    resultRef.current = v
    setResult(v)
  }

  // Per-sandbox view state, so switching sandboxes preserves each one's open
  // file, editor content, terminal output, and result — the terminal and
  // editor belong to a container, not to the app. Keyed by session id, or
  // "scratch" for one-shot. Terminal output is recorded as replayable entries
  // (xterm has no readable buffer), and rewritten on switch.
  type TermEntry =
    | { kind: 'out'; data: string; stream: 'stdout' | 'stderr' }
    | { kind: 'status'; text: string; tone: 'ok' | 'warn' | 'err' }
  interface ViewState {
    openFile: string | null
    code: string
    term: TermEntry[]
    result: ExecResult | null
  }
  const views = useRef<Record<string, ViewState>>({})
  const keyOf = (id: string | null) => id ?? 'scratch'

  // Terminal writes go through these so they also land in the current
  // sandbox's replay buffer. Keyed off the ref, which is always current.
  function termOut(data: string, stream: 'stdout' | 'stderr') {
    term.current?.write(data, stream)
    views.current[keyOf(sessionIdRef.current)]?.term.push({ kind: 'out', data, stream })
  }
  function termStatus(text: string, tone: 'ok' | 'warn' | 'err' = 'ok') {
    term.current?.writeStatus(text, tone)
    views.current[keyOf(sessionIdRef.current)]?.term.push({ kind: 'status', text, tone })
  }
  function termReset(key: string) {
    if (!views.current[key]) {
      views.current[key] = { openFile: null, code: '', term: [], result: null }
    }
    views.current[key].term = []
    term.current?.clear()
  }
  function replayTerm(entries: TermEntry[]) {
    term.current?.clear()
    for (const e of entries) {
      if (e.kind === 'out') term.current?.write(e.data, e.stream)
      else term.current?.writeStatus(e.text, e.tone)
    }
  }

  const onTermReady = useCallback((h: TerminalHandle) => {
    term.current = h
    views.current['scratch'] ??= { openFile: null, code: '', term: [], result: null }
    h.writeStatus('sandbox ready — ⌘↵ or Run', 'ok')
  }, [])

  const refreshFiles = useCallback(async (sid: string) => {
    try {
      setRepoFiles(await listRepoFiles(sid))
    } catch {
      /* ignore */
    }
  }, [])

  // ensureSession returns the current session id, creating a sandbox first if
  // we're on scratch — so "new file" and upload work without a manual step.
  async function ensureSession(): Promise<string> {
    if (sessionId) return sessionId
    const id = await createSession(toApiLimits(limits))
    sessionIdRef.current = id
    views.current[id] ??= { openFile: null, code: '', term: [], result: null }
    setSessionId(id)
    return id
  }

  // Create a blank file (or open it if it already exists) and focus it.
  async function newFile(path: string) {
    try {
      const sid = await ensureSession()
      const existing = await listRepoFiles(sid)
      if (!existing.includes(path)) {
        await writeRepoFile(sid, path, '')
      }
      await refreshFiles(sid)
      updateCode('')
      updateRepoFile(path)
      updateResult(null)
    } catch (e) {
      term.current?.writeStatus(`[error] new file: ${e}`, 'err')
    }
  }

  async function deleteFile(path: string) {
    if (!sessionId) return
    try {
      await deleteRepoFile(sessionId, path)
      await refreshFiles(sessionId)
      if (repoFile === path) {
        updateRepoFile(null)
        updateCode('')
      }
    } catch (e) {
      term.current?.writeStatus(`[error] delete: ${e}`, 'err')
    }
  }

  function selectPreset(id: string) {
    const p = PRESETS.find((x) => x.id === id)
    if (!p) return
    setPreset(p)
    updateCode(p.code)
    updateRepoFile(null) // preset code is a snippet, not a repo file
    setLimits(limitsForPreset(p))
    updateResult(null)
    termReset('scratch')
    termStatus(`${p.label} — expect: ${p.expect}`, 'warn')
  }

  // Switching sandbox saves the current sandbox's view (open file, editor
  // content, result — terminal output is already recorded live) and restores
  // the target's, so each container keeps its own editor and terminal.
  function selectSession(id: string | null) {
    // Snapshot the OUTGOING sandbox from refs (always current), not from
    // possibly-stale render state.
    const curKey = keyOf(sessionIdRef.current)
    const cur = (views.current[curKey] ??= { openFile: null, code: '', term: [], result: null })
    cur.openFile = repoFileRef.current
    cur.code = codeRef.current
    cur.result = resultRef.current

    const nextKey = keyOf(id)
    const next = (views.current[nextKey] ??= { openFile: null, code: '', term: [], result: null })

    sessionIdRef.current = id
    setSessionId(id)
    updateRepoFile(next.openFile)
    updateResult(next.result)
    replayTerm(next.term)
    if (next.term.length === 0) {
      term.current?.writeStatus(
        id === null ? 'scratch — one-shot runs' : 'sandbox selected — ⌘↵ or Run',
        'ok',
      )
    }

    if (id === null) {
      setRepoFiles([])
      updateCode(next.openFile ? next.code : preset.code)
    } else {
      refreshFiles(id)
      updateCode(next.code)
    }
  }

  // After an upload, refresh the tree and open the first file. sid is passed
  // explicitly because the upload may have just auto-created the session.
  async function onUploaded(sid: string, firstFile: string | null) {
    await refreshFiles(sid)
    if (firstFile) openFile(firstFile, sid)
  }

  // Open a repo file into the editor. sid is explicit so a just-created session
  // (not yet reflected in state) still works.
  async function openFile(path: string, sid = sessionId) {
    if (!sid) return
    try {
      const content = await readRepoFile(sid, path)
      updateCode(content)
      updateRepoFile(path)
      updateResult(null)
    } catch (e) {
      term.current?.writeStatus(`[error] open ${path}: ${e}`, 'err')
    }
  }

  // Stream an execution's output into the terminal and report the outcome.
  // Output is recorded into the active sandbox's buffer so switching away and
  // back shows this run's results again.
  async function stream(startId: Promise<string>) {
    if (!term.current) return
    updateResult(null)
    setRunning(true)
    termReset(keyOf(sessionId)) // a new run clears this sandbox's terminal only
    try {
      const id = await startId
      const res = await streamExecution(id, (frame) => {
        if (frame.type === 'chunk' && frame.data !== undefined) {
          termOut(frame.data, frame.stream ?? 'stdout')
        }
      })
      updateResult(res)
      views.current[keyOf(sessionIdRef.current)].result = res
      if (res.timedOut) {
        termStatus(`[TIMED OUT] killed after ${res.durationMs}ms`, 'err')
      } else if (res.oomKilled) {
        termStatus(`[OOM-KILLED] exceeded the memory limit`, 'err')
      } else {
        termStatus(
          `[exit ${res.exitCode}] startup ${res.startupMs}ms · run ${res.durationMs}ms`,
          res.exitCode === 0 ? 'ok' : 'warn',
        )
      }
    } catch (e) {
      termStatus(`[error] ${e instanceof Error ? e.message : String(e)}`, 'err')
    } finally {
      setRunning(false)
      setRunCount((n) => n + 1)
    }
  }

  // Save the open repo file back to the container without running it — useful
  // for helper modules that aren't meant to be executed directly.
  async function saveFile() {
    if (!sessionId || !repoFile || running) return
    try {
      await writeRepoFile(sessionId, repoFile, code)
      await refreshFiles(sessionId) // a brand-new file now appears in the tree
      termStatus(`saved ${repoFile}`, 'ok')
    } catch (e) {
      termStatus(`[error] save: ${e}`, 'err')
    }
  }

  // Run button: if a repo file is open, save it first then run it; otherwise
  // run the snippet (one-shot or in the session).
  async function run() {
    if (running) return
    const timeoutMs = limits.timeoutSec * 1000
    if (sessionId && repoFile) {
      // Push the edited file back, then run it from the repo root.
      await writeRepoFile(sessionId, repoFile, code)
      await refreshFiles(sessionId) // a brand-new file now appears in the tree
      await stream(startArgv(sessionId, ['python3', repoFile], timeoutMs))
    } else {
      await stream(startRun(code, timeoutMs, toApiLimits(limits), sessionId))
    }
  }
  runRef.current = run

  // ⌘↵ / Ctrl+↵ runs, the way every code playground does.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
        e.preventDefault()
        runRef.current()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  return (
    <div className="app">
      <header className="app-header">
        <div>
          <h1>sandbox executor</h1>
          <p>
            untrusted Python in a network-isolated container with kernel-enforced
            memory, CPU, process and time limits
          </p>
        </div>
        <a
          className="repo-link"
          href="https://github.com/hanzelhira/sandbox-executor"
          target="_blank"
          rel="noreferrer"
        >
          source
        </a>
      </header>

      <div className={sessionId ? 'layout layout-repo' : 'layout'}>
        {sessionId && (
          <aside className="explorer-col pane">
            <FileExplorer
              files={repoFiles}
              selected={repoFile}
              onSelect={openFile}
              onNewFile={newFile}
              onDelete={deleteFile}
              disabled={running}
              hasSession={sessionId !== null}
            />
          </aside>
        )}

        <main className="main-col">
          {sessionId ? (
            <div className="toolbar">
              <span className="stopped-by">
                {repoFile ? (
                  <>
                    editing <b>{repoFile}</b> — Run saves &amp; executes it
                  </>
                ) : (
                  <>select or create a file to run it</>
                )}
              </span>
            </div>
          ) : (
            <div className="toolbar">
              <label htmlFor="preset">Try an attack</label>
              <select
                id="preset"
                value={preset.id}
                disabled={running}
                onChange={(e) => selectPreset(e.target.value)}
              >
                {PRESETS.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.label}
                  </option>
                ))}
              </select>
              <span className="stopped-by">
                stopped by <b>{preset.stoppedBy}</b>
              </span>
            </div>
          )}

          <div className="pane editor-pane">
            <CodeEditor value={code} onChange={updateCode} readOnly={running} />
          </div>

          <div className="run-row">
            <button className="run-btn" onClick={run} disabled={running}>
              {running ? 'running…' : repoFile ? 'Save & Run' : 'Run'}
              <kbd>⌘↵</kbd>
            </button>

            {repoFile && (
              <button className="save-btn" onClick={saveFile} disabled={running}>
                Save
              </button>
            )}

            {result && (
              <span className="result-strip">
                {/* On a kill the exit code is a sentinel (-1) or a signal
                    artifact (137); the badge is the real information. */}
                {result.timedOut ? (
                  <b className="badge danger">TIMED OUT</b>
                ) : result.oomKilled ? (
                  <b className="badge danger">OOM-KILLED</b>
                ) : (
                  <b className={result.exitCode === 0 ? 'badge ok' : 'badge warn'}>
                    exit {result.exitCode}
                  </b>
                )}
                <span>
                  startup <b>{result.startupMs}ms</b>
                </span>
                <span>
                  run <b>{result.durationMs}ms</b>
                </span>
              </span>
            )}
          </div>

          <div className="pane terminal-pane">
            <TerminalPane onReady={onTermReady} />
          </div>

          <StatsBar refreshKey={runCount} />
        </main>

        <aside className="side-col">
          <section className="pane side-panel">
            <SessionRail
              selected={sessionId}
              onSelect={selectSession}
              limits={toApiLimits(limits)}
              disabled={running}
            />
          </section>
          <section className="pane side-panel">
            <SectionLabel>Load a repo</SectionLabel>
            <RepoPanel ensureSession={ensureSession} onLoaded={onUploaded} disabled={running} />
          </section>
          <section className="pane side-panel">
            <SectionLabel>
              Resource limits{sessionId ? ' (per sandbox — fixed at creation)' : ''}
            </SectionLabel>
            <LimitsPanel
              limits={limits}
              onChange={setLimits}
              disabled={running || sessionId !== null}
            />
          </section>
          <section className="pane side-panel">
            <IsolationPanel />
          </section>
        </aside>
      </div>
    </div>
  )
}
