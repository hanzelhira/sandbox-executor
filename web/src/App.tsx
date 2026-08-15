import { useCallback, useRef, useState } from 'react'
import { startExecution, streamExecution } from './api'
import type { ExecResult } from './api'
import { TerminalPane } from './Terminal'
import type { TerminalHandle } from './Terminal'
import { CodeEditor } from './Editor'
import { LimitsPanel, DEFAULT_LIMITS, toApiLimits } from './Limits'
import type { LimitsState } from './Limits'
import { PRESETS, limitsForPreset } from './presets'
import type { Preset } from './presets'

export default function App() {
  const [preset, setPreset] = useState<Preset>(PRESETS[0])
  const [code, setCode] = useState(PRESETS[0].code)
  const [limits, setLimits] = useState<LimitsState>(DEFAULT_LIMITS)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [running, setRunning] = useState(false)
  const term = useRef<TerminalHandle | null>(null)

  const onTermReady = useCallback((h: TerminalHandle) => {
    term.current = h
    h.writeStatus('sandbox ready — press Run', 'ok')
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
    if (!t) return
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
    }
  }

  return (
    <div style={{ maxWidth: 1180, margin: '0 auto', padding: '24px 20px 40px' }}>
      <header style={{ marginBottom: 18 }}>
        <h1 style={{ margin: 0, fontSize: 17, fontWeight: 600 }}>
          sandbox executor
        </h1>
        <p style={{ margin: '4px 0 0', fontSize: 12, color: 'var(--muted)' }}>
          untrusted Python in a network-isolated container with kernel-enforced
          memory, CPU, process and time limits
        </p>
      </header>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 220px', gap: 16 }}>
        {/* left: editor + terminal */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12, minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <label htmlFor="preset" style={{ fontSize: 12, color: 'var(--muted)' }}>
              Try an attack
            </label>
            <select
              id="preset"
              value={preset.id}
              disabled={running}
              onChange={(e) => selectPreset(e.target.value)}
              style={{
                background: 'var(--panel)',
                color: 'var(--fg)',
                border: '1px solid var(--border)',
                borderRadius: 6,
                padding: '5px 8px',
                fontFamily: 'inherit',
                fontSize: 12,
              }}
            >
              {PRESETS.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.label}
                </option>
              ))}
            </select>
            <span style={{ fontSize: 11, color: 'var(--muted)' }}>
              stopped by <b style={{ color: 'var(--fg)' }}>{preset.stoppedBy}</b>
            </span>
          </div>

          <div
            style={{
              height: 280,
              border: '1px solid var(--border)',
              borderRadius: 8,
              overflow: 'hidden',
              background: 'var(--panel)',
            }}
          >
            <CodeEditor value={code} onChange={setCode} readOnly={running} />
          </div>

          <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
            <button
              onClick={run}
              disabled={running}
              style={{
                background: running ? 'var(--border)' : 'var(--accent)',
                color: '#fff',
                border: 'none',
                borderRadius: 6,
                padding: '8px 20px',
                fontFamily: 'inherit',
                fontSize: 13,
                fontWeight: 600,
                cursor: running ? 'default' : 'pointer',
              }}
            >
              {running ? 'running…' : 'Run'}
            </button>

            {result && (
              <span style={{ fontSize: 12, color: 'var(--muted)' }}>
                {/* On a kill, the exit code is a sentinel (-1) or a signal
                    artifact (137) — the badge is the real information. */}
                {result.timedOut ? (
                  <b style={{ color: 'var(--danger)' }}>TIMED OUT</b>
                ) : result.oomKilled ? (
                  <b style={{ color: 'var(--danger)' }}>OOM-KILLED</b>
                ) : (
                  <span
                    style={{ color: result.exitCode === 0 ? '#3fb950' : '#d29922' }}
                  >
                    exit {result.exitCode}
                  </span>
                )}
                {' · '}startup {result.startupMs}ms · run {result.durationMs}ms
              </span>
            )}
          </div>

          <div
            style={{
              height: 300,
              background: 'var(--panel)',
              border: '1px solid var(--border)',
              borderRadius: 8,
              padding: 8,
            }}
          >
            <TerminalPane onReady={onTermReady} />
          </div>
        </div>

        {/* right: limits */}
        <aside
          style={{
            border: '1px solid var(--border)',
            borderRadius: 8,
            padding: 14,
            background: 'var(--panel)',
            alignSelf: 'start',
          }}
        >
          <LimitsPanel limits={limits} onChange={setLimits} disabled={running} />
        </aside>
      </div>
    </div>
  )
}
