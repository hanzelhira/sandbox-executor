import { useCallback, useEffect, useState } from 'react'
import { createSession, listSessions, destroySession } from './api'
import type { SessionInfo, Limits } from './api'
import { SectionLabel } from './Isolation'

// The session rail. "scratch" (one-shot) is the default pseudo-entry and stays
// selected until the user creates a persistent sandbox.

interface Props {
  selected: string | null // null = one-shot scratch
  onSelect: (id: string | null) => void
  limits: Limits
  disabled: boolean
}

export function SessionRail({ selected, onSelect, limits, disabled }: Props) {
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [max, setMax] = useState(0)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(() => {
    listSessions()
      .then((r) => {
        setSessions(r.sessions)
        setMax(r.max)
      })
      .catch(() => {})
  }, [])

  useEffect(() => {
    refresh()
    const t = setInterval(refresh, 4000)
    return () => clearInterval(t)
  }, [refresh])

  async function create() {
    setError(null)
    try {
      const id = await createSession(limits)
      onSelect(id)
      refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  async function destroy(id: string) {
    await destroySession(id)
    if (selected === id) onSelect(null)
    refresh()
  }

  const atCap = max > 0 && sessions.length >= max

  return (
    <div>
      <SectionLabel>Sandboxes</SectionLabel>

      <button
        className={`session-row ${selected === null ? 'active' : ''}`}
        onClick={() => onSelect(null)}
      >
        <span className="session-name">scratch</span>
        <span className="session-meta">one-shot</span>
      </button>

      {sessions.map((s) => (
        <div
          key={s.id}
          className={`session-row ${selected === s.id ? 'active' : ''}`}
          onClick={() => onSelect(s.id)}
        >
          <span className="session-dot" data-healthy={s.healthy} />
          <span className="session-name">{s.id.slice(0, 8)}</span>
          <span className="session-meta">{s.runs} runs</span>
          <button
            className="session-x"
            title="destroy sandbox"
            onClick={(e) => {
              e.stopPropagation()
              destroy(s.id)
            }}
          >
            ✕
          </button>
        </div>
      ))}

      <button
        className="new-session-btn"
        onClick={create}
        disabled={disabled || atCap}
        title={atCap ? 'session limit reached' : 'create a persistent sandbox'}
      >
        + New sandbox
      </button>
      {error && <div className="session-error">{error}</div>}
      {selected !== null && (
        <p className="session-note">
          State persists between runs in this sandbox. Isolation from other
          sandboxes stays absolute.
        </p>
      )}
    </div>
  )
}
