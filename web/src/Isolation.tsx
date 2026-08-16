import { useEffect, useState } from 'react'
import { fetchConfig, fetchStats } from './api'
import type { Config, Stats } from './api'

// Everything here is read from the live server config rather than written by
// hand, so the panel cannot claim a protection that is not actually switched on.

export function IsolationPanel() {
  const [config, setConfig] = useState<Config | null>(null)

  useEffect(() => {
    fetchConfig().then(setConfig).catch(() => setConfig(null))
  }, [])

  if (!config) return null

  return (
    <div>
      <SectionLabel>Active isolation</SectionLabel>
      <div style={{ fontSize: 11, color: 'var(--muted)', marginBottom: 10 }}>
        {config.image} · {config.runtime}
      </div>
      <dl style={{ margin: 0, display: 'grid', gap: 7 }}>
        {config.controls.map((c) => (
          <div key={c.name} title={`defends against: ${c.defends}`}>
            <dt
              style={{
                fontSize: 11,
                color: 'var(--muted)',
                display: 'flex',
                justifyContent: 'space-between',
                gap: 8,
              }}
            >
              <span>{c.name}</span>
              <span
                style={{
                  color: c.value === 'DISABLED' ? 'var(--danger)' : '#3fb950',
                  textAlign: 'right',
                }}
              >
                {c.value}
              </span>
            </dt>
          </div>
        ))}
      </dl>
    </div>
  )
}

// Startup latency is the project's headline metric; showing it live means the
// demo does not depend on quoting a number from a README.
export function StatsBar({ refreshKey }: { refreshKey: number }) {
  const [stats, setStats] = useState<Stats | null>(null)

  useEffect(() => {
    fetchStats().then(setStats).catch(() => {})
  }, [refreshKey])

  if (!stats || stats.count === 0) return null

  return (
    <div
      style={{
        display: 'flex',
        flexWrap: 'wrap',
        gap: 16,
        fontSize: 11,
        color: 'var(--muted)',
        borderTop: '1px solid var(--border)',
        paddingTop: 10,
        marginTop: 2,
      }}
    >
      <Metric label="runs" value={String(stats.count)} />
      <Metric label="startup p50" value={`${stats.startupP50Ms} ms`} />
      <Metric label="startup p99" value={`${stats.startupP99Ms} ms`} />
      <Metric label="total p50" value={`${stats.totalP50Ms} ms`} />
      <Metric
        label="capacity"
        value={`${stats.running}/${stats.maxConcurrent} running`}
      />
    </div>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <span>
      {label} <b style={{ color: 'var(--fg)', fontWeight: 600 }}>{value}</b>
    </span>
  )
}

export function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div
      style={{
        fontSize: 10,
        textTransform: 'uppercase',
        letterSpacing: 0.7,
        color: 'var(--muted)',
        marginBottom: 10,
      }}
    >
      {children}
    </div>
  )
}
