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

// Runs an arbitrary argv (e.g. ["python3","main.py"]) in a session's repo dir.
export async function startArgv(
  sessionId: string,
  argv: string[],
  timeoutMs: number,
): Promise<string> {
  const resp = await fetch(`/sessions/${sessionId}/exec`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` },
    body: JSON.stringify({ argv, timeoutMs }),
  })
  if (!resp.ok) throw new Error(`exec: ${resp.status} ${await resp.text()}`)
  return ((await resp.json()) as { executionId: string }).executionId
}

export interface UploadFile {
  path: string
  data: string // base64
}

export async function uploadRepo(
  sessionId: string,
  files: UploadFile[],
  label: string,
): Promise<string[]> {
  const resp = await fetch(`/sessions/${sessionId}/files`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${TOKEN}` },
    body: JSON.stringify({ label, files }),
  })
  if (!resp.ok) throw new Error(await resp.text())
  return ((await resp.json()) as { files: string[] }).files
}

export const listRepoFiles = (sessionId: string) =>
  getJSON<{ files: string[] }>(`/sessions/${sessionId}/files`).then((r) => r.files)

export const readRepoFile = (sessionId: string, path: string) =>
  getJSON<{ path: string; content: string }>(
    `/sessions/${sessionId}/files?path=${encodeURIComponent(path)}`,
  ).then((r) => r.content)

// Create or overwrite a single file (empty content creates a blank file).
export async function writeRepoFile(
  sessionId: string,
  path: string,
  content: string,
): Promise<void> {
  const b64 = btoa(unescape(encodeURIComponent(content)))
  await uploadRepo(sessionId, [{ path, data: b64 }], 'edit')
}

export async function deleteRepoFile(sessionId: string, path: string): Promise<void> {
  const resp = await fetch(
    `/sessions/${sessionId}/files?path=${encodeURIComponent(path)}`,
    { method: 'DELETE', headers: { Authorization: `Bearer ${TOKEN}` } },
  )
  if (!resp.ok && resp.status !== 204) throw new Error(await resp.text())
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
