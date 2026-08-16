// Client for the sandbox-executor API. Mirrors the Go wire types exactly
// (internal/api/server.go streamFrame / executor.ExecResult).

export interface ExecResult {
  exitCode: number
  timedOut: boolean
  oomKilled: boolean
  durationMs: number
  startupMs: number
}

export interface StreamFrame {
  type: 'chunk' | 'result' | 'error'
  stream?: 'stdout' | 'stderr'
  data?: string
  ts?: string
  result?: ExecResult
  error?: string
}

export interface Limits {
  memoryBytes?: number
  cpuQuota?: number
  pidsMax?: number
}

// Dev-only token, matching SANDBOX_TOKEN on the Go server. A real deployment
// would issue per-user tokens; static bearer auth is the Week 1 scope.
const TOKEN = import.meta.env.VITE_SANDBOX_TOKEN ?? 'devtoken'

export interface Control {
  name: string
  value: string
  defends: string
}

export interface Config {
  backend: string
  image: string
  runtime: string
  controls: Control[]
  maxConcurrent: number
}

export interface Stats {
  count: number
  startupP50Ms: number
  startupP99Ms: number
  totalP50Ms: number
  totalP99Ms: number
  inFlight: number
  running: number
  maxConcurrent: number
}

async function getJSON<T>(path: string): Promise<T> {
  const resp = await fetch(path, { headers: { Authorization: `Bearer ${TOKEN}` } })
  if (!resp.ok) throw new Error(`${path}: ${resp.status}`)
  return (await resp.json()) as T
}

export const fetchConfig = () => getJSON<Config>('/config')
export const fetchStats = () => getJSON<Stats>('/stats')

export interface SessionInfo {
  id: string
  createdAt: string
  idleMs: number
  runs: number
  healthy: boolean
}

export async function createSession(limits: Limits): Promise<string> {
  const resp = await fetch('/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` },
    body: JSON.stringify({ limits }),
  })
  if (resp.status === 429) throw new Error('session limit reached')
  if (!resp.ok) throw new Error(`create session: ${resp.status}`)
  return ((await resp.json()) as { sessionId: string }).sessionId
}

export const listSessions = () =>
  getJSON<{ sessions: SessionInfo[]; max: number }>('/sessions')

export async function destroySession(id: string): Promise<void> {
  await fetch(`/sessions/${id}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${TOKEN}` },
  })
}

// Runs code either one-shot (sessionId null) or inside a session.
export async function startRun(
  code: string,
  timeoutMs: number,
  limits: Limits,
  sessionId: string | null,
): Promise<string> {
  if (sessionId) {
    const resp = await fetch(`/sessions/${sessionId}/exec`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` },
      body: JSON.stringify({ code, timeoutMs }),
    })
    if (!resp.ok) throw new Error(`exec: ${resp.status} ${await resp.text()}`)
    return ((await resp.json()) as { executionId: string }).executionId
  }
  return startExecution(code, timeoutMs, limits)
}

export async function startExecution(
  code: string,
  timeoutMs: number,
  limits: Limits,
): Promise<string> {
  const resp = await fetch('/execute', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${TOKEN}`,
    },
    body: JSON.stringify({ language: 'python', code, timeoutMs, limits }),
  })
  if (!resp.ok) {
    throw new Error(`execute failed: ${resp.status} ${await resp.text()}`)
  }
  const body = (await resp.json()) as { executionId: string }
  return body.executionId
}

// Opens the output stream for an execution. Calls onFrame for every frame;
// resolves with the final result (or rejects) when the stream ends.
export function streamExecution(
  executionId: string,
  onFrame: (f: StreamFrame) => void,
): Promise<ExecResult> {
  return new Promise((resolve, reject) => {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    // Browsers can't set headers on WebSocket dials, hence ?token=.
    const ws = new WebSocket(
      `${proto}://${location.host}/stream/${executionId}?token=${TOKEN}`,
    )
    let settled = false

    ws.onmessage = (ev) => {
      const frame = JSON.parse(ev.data as string) as StreamFrame
      onFrame(frame)
      if (frame.type === 'result' && frame.result) {
        settled = true
        resolve(frame.result)
        ws.close()
      } else if (frame.type === 'error') {
        settled = true
        reject(new Error(frame.error ?? 'execution failed'))
        ws.close()
      }
    }
    ws.onerror = () => {
      if (!settled) {
        settled = true
        reject(new Error('websocket error'))
      }
    }
    ws.onclose = () => {
      if (!settled) {
        settled = true
        reject(new Error('stream closed before result frame'))
      }
    }
  })
}
