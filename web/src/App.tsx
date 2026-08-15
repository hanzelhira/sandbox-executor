import { useCallback, useRef, useState } from 'react'
import { startExecution, streamExecution } from './api'
import type { ExecResult } from './api'
import { TerminalPane } from './Terminal'
import type { TerminalHandle } from './Terminal'

const DEFAULT_CODE = `import time
for i in range(5):
    print("tick", i)
    time.sleep(0.4)
print("done")
`

export default function App() {
  const [code, setCode] = useState(DEFAULT_CODE)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [running, setRunning] = useState(false)
  const term = useRef<TerminalHandle | null>(null)

  const onTermReady = useCallback((h: TerminalHandle) => {
    term.current = h
    h.writeStatus('sandbox ready — press Run', 'ok')
  }, [])

  async function run() {
    const t = term.current
    if (!t) return
    setResult(null)
    setRunning(true)
    t.clear()
    const started = performance.now()
    try {
      const id = await startExecution(code, 15000, {})
      const res = await streamExecution(id, (frame) => {
        if (frame.type === 'chunk' && frame.data !== undefined) {
          t.write(frame.data, frame.stream ?? 'stdout')
        }
      })
      setResult(res)
      const wall = Math.round(performance.now() - started)
      if (res.timedOut) {
        t.writeStatus(`[TIMED OUT] killed after ${res.durationMs}ms`, 'err')
      } else if (res.oomKilled) {
        t.writeStatus(`[OOM-KILLED] exceeded memory limit`, 'err')
      } else {
        t.writeStatus(
          `[exit ${res.exitCode}] startup ${res.startupMs}ms · run ${res.durationMs}ms · wall ${wall}ms`,
          res.exitCode === 0 ? 'ok' : 'warn',
        )
      }
    } catch (e) {
      t.writeStatus(`[error] ${e instanceof Error ? e.message : String(e)}`, 'err')
    } finally {
      setRunning(false)
    }
  }

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 12,
        maxWidth: 860,
        margin: '2rem auto',
        padding: '0 1rem',
        fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
        color: '#c9d1d9',
      }}
    >
      <h2 style={{ margin: 0, fontSize: 16 }}>sandbox executor</h2>

      <textarea
        value={code}
        onChange={(e) => setCode(e.target.value)}
        rows={9}
        spellCheck={false}
        style={{
          width: '100%',
          fontFamily: 'inherit',
          fontSize: 13,
          background: '#0b0e14',
          color: '#c9d1d9',
          border: '1px solid #21262d',
          borderRadius: 6,
          padding: 10,
          resize: 'vertical',
        }}
      />

      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          onClick={run}
          disabled={running}
          style={{
            background: running ? '#21262d' : '#238636',
            color: '#fff',
            border: 'none',
            borderRadius: 6,
            padding: '7px 16px',
            fontFamily: 'inherit',
            fontSize: 13,
            cursor: running ? 'default' : 'pointer',
          }}
        >
          {running ? 'running…' : 'Run'}
        </button>
        {result && (
          <span style={{ fontSize: 12, color: '#8b949e' }}>
            {/* When the run was killed, the exit code is a sentinel (-1) or a
                signal artifact (137) — the badge is the real information, so
                the number is suppressed rather than shown as noise. */}
            {result.timedOut ? (
              <b style={{ color: '#f85149' }}>TIMED OUT</b>
            ) : result.oomKilled ? (
              <b style={{ color: '#f85149' }}>OOM-KILLED</b>
            ) : (
              <span style={{ color: result.exitCode === 0 ? '#3fb950' : '#d29922' }}>
                exit {result.exitCode}
              </span>
            )}
            {' · '}startup {result.startupMs}ms · run {result.durationMs}ms
          </span>
        )}
      </div>

      <div
        style={{
          height: 320,
          background: '#0b0e14',
          border: '1px solid #21262d',
          borderRadius: 6,
          padding: 8,
        }}
      >
        <TerminalPane onReady={onTermReady} />
      </div>
    </div>
  )
}
