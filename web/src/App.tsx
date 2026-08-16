import { useCallback, useEffect, useRef, useState } from 'react'
import { startExecution, streamExecution } from './api'
import type { ExecResult } from './api'
import { TerminalPane } from './Terminal'
import type { TerminalHandle } from './Terminal'
import { CodeEditor } from './Editor'
import { LimitsPanel, DEFAULT_LIMITS, toApiLimits } from './Limits'
import type { LimitsState } from './Limits'
import { PRESETS, limitsForPreset } from './presets'
import type { Preset } from './presets'
import { IsolationPanel, StatsBar, SectionLabel } from './Isolation'

export default function App() {
  const [preset, setPreset] = useState<Preset>(PRESETS[0])
  const [code, setCode] = useState(PRESETS[0].code)
  const [limits, setLimits] = useState<LimitsState>(DEFAULT_LIMITS)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [running, setRunning] = useState(false)
  const [runCount, setRunCount] = useState(0)
  const term = useRef<TerminalHandle | null>(null)
  // Kept in a ref so the keyboard shortcut always calls the current closure
  // without re-binding the listener on every state change.
  const runRef = useRef<() => void>(() => {})

  const onTermReady = useCallback((h: TerminalHandle) => {
    term.current = h
    h.writeStatus('sandbox ready — ⌘↵ or Run', 'ok')
  }, [])

  function selectPreset(id: string) {
    const p = PRESETS.find((x) => x.id === id)
    if (!p) return
    setPreset(p)
    setCode(p.code)
    // Presets carry the limits that make their point clearest — a memory bomb
    // is more legible against 64 MB than 256 MB.
    setLimits(limitsForPreset(p))
    setResult(null)
    term.current?.clear()
    term.current?.writeStatus(`${p.label} — expect: ${p.expect}`, 'warn')
  }

  async function run() {
    const t = term.current
    if (!t || running) return
    setResult(null)
    setRunning(true)
    t.clear()
    try {
      const id = await startExecution(
        code,
        limits.timeoutSec * 1000,
        toApiLimits(limits),
      )
      const res = await streamExecution(id, (frame) => {
        if (frame.type === 'chunk' && frame.data !== undefined) {
          t.write(frame.data, frame.stream ?? 'stdout')
        }
      })
      setResult(res)
      if (res.timedOut) {
        t.writeStatus(
          `[TIMED OUT] killed after ${res.durationMs}ms (limit ${limits.timeoutSec}s)`,
          'err',
        )
      } else if (res.oomKilled) {
        t.writeStatus(
          `[OOM-KILLED] exceeded the ${limits.memoryMB} MB memory limit`,
          'err',
        )
      } else {
        t.writeStatus(
          `[exit ${res.exitCode}] startup ${res.startupMs}ms · run ${res.durationMs}ms`,
          res.exitCode === 0 ? 'ok' : 'warn',
        )
      }
    } catch (e) {
      t.writeStatus(`[error] ${e instanceof Error ? e.message : String(e)}`, 'err')
    } finally {
      setRunning(false)
      setRunCount((n) => n + 1)
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

      <div className="layout">
        <main className="main-col">
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

          <div className="pane editor-pane">
            <CodeEditor value={code} onChange={setCode} readOnly={running} />
          </div>

          <div className="run-row">
            <button className="run-btn" onClick={run} disabled={running}>
              {running ? 'running…' : 'Run'}
              <kbd>⌘↵</kbd>
            </button>

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
            <SectionLabel>Resource limits</SectionLabel>
            <LimitsPanel limits={limits} onChange={setLimits} disabled={running} />
          </section>
          <section className="pane side-panel">
            <IsolationPanel />
          </section>
        </aside>
      </div>
    </div>
  )
}
