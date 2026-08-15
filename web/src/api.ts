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
