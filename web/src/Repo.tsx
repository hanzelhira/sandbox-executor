import { useCallback, useEffect, useRef, useState } from 'react'
import { uploadRepo, listRepoFiles } from './api'
import type { UploadFile } from './api'

// Reads a browser File into {path, base64}. Uses the webkitRelativePath when a
// folder was picked, so the tree structure is preserved.
function readFile(file: File, overridePath?: string): Promise<UploadFile> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error)
    reader.onload = () => {
      const res = reader.result as string
      // strip "data:...;base64,"
      const b64 = res.slice(res.indexOf(',') + 1)
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const rel = overridePath ?? (file as any).webkitRelativePath ?? file.name
      resolve({ path: rel || file.name, data: b64 })
    }
    reader.readAsDataURL(file)
  })
}

// Drop the leading top-level folder name a folder-picker prepends, so paths are
// repo-relative (myrepo/src/x.py -> src/x.py).
function stripTopDir(files: UploadFile[]): UploadFile[] {
  const top = files[0]?.path.split('/')[0]
  if (!top || !files.every((f) => f.path.startsWith(top + '/'))) return files
  return files.map((f) => ({ ...f, path: f.path.slice(top.length + 1) }))
}

interface Props {
  sessionId: string | null
  selectedFile: string | null
  onSelectFile: (path: string) => void
  onLoaded: () => void
  disabled: boolean
}

export function RepoPanel({ sessionId, selectedFile, onSelectFile, onLoaded, disabled }: Props) {
  const [files, setFiles] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const folderInput = useRef<HTMLInputElement>(null)
  const zipInput = useRef<HTMLInputElement>(null)

  const refresh = useCallback(() => {
    if (!sessionId) {
      setFiles([])
      return
    }
    listRepoFiles(sessionId)
      .then((f) => {
        setFiles(f)
        // A session selected after reload already has files; signal the parent
        // so the command bar appears without needing a fresh upload.
        if (f.length > 0) onLoaded()
      })
      .catch(() => {})
  }, [sessionId, onLoaded])

  useEffect(refresh, [refresh])

  async function handleFolder(fileList: FileList | null) {
    if (!fileList || !sessionId) return
    setBusy(true)
    setError(null)
    try {
      const uploads = stripTopDir(await Promise.all(Array.from(fileList).map((f) => readFile(f))))
      const paths = await uploadRepo(sessionId, uploads, 'folder')
      setFiles(paths)
      onLoaded()
      if (paths[0]) onSelectFile(paths.find((p) => p.endsWith('.py')) ?? paths[0])
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  if (!sessionId) {
    return (
      <p className="repo-hint">
        Create a sandbox, then upload a folder or zip to load a repo.
      </p>
    )
  }

  return (
    <div className="repo-panel">
      <div className="repo-actions">
        <button
          className="repo-btn"
          disabled={disabled || busy}
          onClick={() => folderInput.current?.click()}
        >
          Upload folder
        </button>
        <button
          className="repo-btn"
          disabled={disabled || busy}
          onClick={() => zipInput.current?.click()}
        >
          Upload .zip
        </button>
        <input
          ref={folderInput}
          type="file"
          hidden
          // @ts-expect-error non-standard but widely supported
          webkitdirectory=""
          directory=""
          multiple
          onChange={(e) => handleFolder(e.target.files)}
        />
        <input
          ref={zipInput}
          type="file"
          hidden
          accept=".zip,application/zip"
          onChange={(e) => handleFolder(e.target.files)}
        />
      </div>

      {/* git field is present but disabled — the Option-A seam */}
      <div className="repo-git" title="git clone needs network; not enabled in this build">
        <input disabled placeholder="git URL (coming soon — needs network)" />
      </div>

      {busy && <div className="repo-status">uploading…</div>}
      {error && <div className="repo-error">{error}</div>}

      <div className="repo-tree">
        {files.length === 0 && !busy && <div className="repo-empty">no files yet</div>}
        {files.map((p) => (
          <button
            key={p}
            className={`repo-file ${selectedFile === p ? 'active' : ''}`}
            onClick={() => onSelectFile(p)}
          >
            {p}
          </button>
        ))}
      </div>
    </div>
  )
}
