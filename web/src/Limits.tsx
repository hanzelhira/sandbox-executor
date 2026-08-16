import type { Limits } from './api'

// Server-side clamps (internal/api/server.go). Mirrored here so the UI can't
// offer values the API would silently reduce — the slider maxima are the
// real maxima.
export const LIMIT_BOUNDS = {
  memoryMB: { min: 16, max: 1024, step: 16 },
  cpu: { min: 0.1, max: 2, step: 0.1 },
  timeoutSec: { min: 1, max: 60, step: 1 },
  pids: { min: 8, max: 256, step: 8 },
} as const

export interface LimitsState {
  memoryMB: number
  cpu: number
  timeoutSec: number
  pids: number
}

export const DEFAULT_LIMITS: LimitsState = {
  memoryMB: 256, // matches executor.DefaultLimits()
  cpu: 0.5,
  timeoutSec: 15,
  pids: 64,
}

export function toApiLimits(l: LimitsState): Limits {
  return {
    memoryBytes: l.memoryMB * 1024 * 1024,
    cpuQuota: l.cpu,
    pidsMax: l.pids,
  }
}

interface SliderProps {
  label: string
  value: number
  display: string
  bounds: { min: number; max: number; step: number }
  onChange: (v: number) => void
  disabled?: boolean
  hint: string
}

function Slider({ label, value, display, bounds, onChange, disabled, hint }: SliderProps) {
  return (
    <label style={{ display: 'block', marginBottom: 12 }} title={hint}>
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          fontSize: 11,
          color: 'var(--muted)',
          marginBottom: 4,
        }}
      >
        <span>{label}</span>
        <span style={{ color: 'var(--fg)' }}>{display}</span>
      </div>
      <input
        type="range"
        min={bounds.min}
        max={bounds.max}
        step={bounds.step}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(Number(e.target.value))}
        style={{ width: '100%', accentColor: 'var(--accent)' }}
      />
    </label>
  )
}

interface Props {
  limits: LimitsState
  onChange: (l: LimitsState) => void
  disabled?: boolean
}

export function LimitsPanel({ limits, onChange, disabled }: Props) {
  const set = <K extends keyof LimitsState>(key: K, v: LimitsState[K]) =>
    onChange({ ...limits, [key]: v })

  return (
    <div>
      <Slider
        label="Memory"
        value={limits.memoryMB}
        display={`${limits.memoryMB} MB`}
        bounds={LIMIT_BOUNDS.memoryMB}
        onChange={(v) => set('memoryMB', v)}
        disabled={disabled}
        hint="cgroups memory.max — exceeding it triggers the kernel OOM killer"
      />
      <Slider
        label="CPU"
        value={limits.cpu}
        display={`${limits.cpu.toFixed(1)} cores`}
        bounds={LIMIT_BOUNDS.cpu}
        onChange={(v) => set('cpu', v)}
        disabled={disabled}
        hint="cgroups cpu.max — caps scheduler time, not correctness"
      />
      <Slider
        label="Timeout"
        value={limits.timeoutSec}
        display={`${limits.timeoutSec}s`}
        bounds={LIMIT_BOUNDS.timeoutSec}
        onChange={(v) => set('timeoutSec', v)}
        disabled={disabled}
        hint="deadline — the whole container is SIGKILLed when it expires"
      />
      <Slider
        label="Max processes"
        value={limits.pids}
        display={String(limits.pids)}
        bounds={LIMIT_BOUNDS.pids}
        onChange={(v) => set('pids', v)}
        disabled={disabled}
        hint="cgroups pids.max — fork() returns EAGAIN past this"
      />

      <button
        onClick={() => onChange(DEFAULT_LIMITS)}
        disabled={disabled}
        style={{
          background: 'transparent',
          color: 'var(--muted)',
          border: '1px solid var(--border)',
          borderRadius: 6,
          padding: '4px 10px',
          fontFamily: 'inherit',
          fontSize: 11,
          cursor: disabled ? 'default' : 'pointer',
        }}
      >
        reset defaults
      </button>
    </div>
  )
}
