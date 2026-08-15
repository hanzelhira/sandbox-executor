import Editor from '@monaco-editor/react'

interface Props {
  value: string
  onChange: (v: string) => void
  readOnly?: boolean
}

export function CodeEditor({ value, onChange, readOnly }: Props) {
  return (
    <Editor
      height="100%"
      defaultLanguage="python"
      theme="vs-dark"
      value={value}
      onChange={(v) => onChange(v ?? '')}
      loading={
        <span style={{ color: 'var(--muted)', fontSize: 12 }}>loading editor…</span>
      }
      options={{
        readOnly,
        fontSize: 13,
        fontFamily:
          'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
        minimap: { enabled: false }, // too narrow a pane for it to help
        scrollBeyondLastLine: false,
        lineNumbers: 'on',
        renderLineHighlight: 'line',
        automaticLayout: true, // re-measures when the pane resizes
        tabSize: 4, // PEP 8
        insertSpaces: true, // never tabs in Python
        padding: { top: 10, bottom: 10 },
        scrollbar: { verticalScrollbarSize: 8, horizontalScrollbarSize: 8 },
        overviewRulerLanes: 0,
      }}
    />
  )
}
