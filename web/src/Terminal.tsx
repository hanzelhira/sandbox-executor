import { useEffect, useRef } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

// ANSI escape sequences. The container's output is raw bytes; we add colour
// ourselves for stderr and for our own status lines, since a plain
// `python3 -c` writing to a pipe emits no colour of its own.
const ANSI = {
  reset: '\x1b[0m',
  red: '\x1b[31m',
  dim: '\x1b[2m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
} as const

export interface TerminalHandle {
  write: (data: string, stream: 'stdout' | 'stderr') => void
  writeStatus: (text: string, tone?: 'ok' | 'warn' | 'err') => void
  clear: () => void
}

interface Props {
  onReady: (handle: TerminalHandle) => void
}

export function TerminalPane({ onReady }: Props) {
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<XTerm | null>(null)

  useEffect(() => {
    if (!hostRef.current) return

    const term = new XTerm({
      convertEol: true, // containers emit \n; a terminal wants \r\n
      cursorBlink: false,
      disableStdin: true, // output-only for now (stdin would need a bidi stream)
      fontSize: 13,
      fontFamily:
        'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
      theme: {
        background: '#0b0e14',
        foreground: '#c9d1d9',
        cursor: '#0b0e14',
        selectionBackground: '#2d3748',
      },
      scrollback: 5000,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(hostRef.current)
    // Defer the first fit: on mount the host has not been laid out yet, so
    // fitting immediately computes a tiny column count and wraps output.
    requestAnimationFrame(() => fit.fit())
    termRef.current = term

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)
    // The pane also changes size when its container does, not just the window.
    const ro = new ResizeObserver(() => fit.fit())
    ro.observe(hostRef.current)

    onReady({
      write: (data, stream) => {
        // stderr in red. Reset after each chunk so a chunk boundary can't
        // leak colour into subsequent stdout.
        if (stream === 'stderr') term.write(ANSI.red + data + ANSI.reset)
        else term.write(data)
      },
      writeStatus: (text, tone = 'ok') => {
        const colour =
          tone === 'err' ? ANSI.red : tone === 'warn' ? ANSI.yellow : ANSI.dim
        term.write(`\r\n${colour}${text}${ANSI.reset}\r\n`)
      },
      clear: () => term.clear(),
    })

    return () => {
      window.removeEventListener('resize', onResize)
      ro.disconnect()
      term.dispose()
    }
    // onReady is called once on mount by design; the parent holds the handle.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return <div ref={hostRef} style={{ height: '100%', width: '100%' }} />
}
