import { useRef, useState } from 'react'
import { uploadRepo } from './api'
import type { UploadFile } from './api'

// Reads a browser File into {path, base64}. Uses webkitRelativePath when a
// folder was picked, so the tree structure is preserved.
function readFile(file: File): Promise<UploadFile> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error)
    reader.onload = () => {
      const res = reader.result as string
      const b64 = res.slice(res.indexOf(',') + 1)
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const rel = (file as any).webkitRelativePath || file.name
      resolve({ path: rel, data: b64 })
    }
    reader.readAsDataURL(file)
  })
}

// Drop the leading top-level folder a folder-picker prepends, so paths are
// repo-relative (myrepo/src/x.py -> src/x.py).
function stripTopDir(files: UploadFile[]): UploadFile[] {
  const top = files[0]?.path.split('/')[0]
  if (!top || !files.every((f) => f.path.startsWith(top + '/'))) return files
  return files.map((f) => ({ ...f, path: f.path.slice(top.length + 1) }))
}

interface Props {
  // ensureSession returns a session id, creating one if needed.
  ensureSession: () => Promise<string>
  onLoaded: (sessionId: string, firstFile: string | null) => void
  disabled: boolean
}

// The right-hand "load a repo" panel: folder/zip upload plus the disabled git
// field. The file tree lives in the left explorer now.
export function RepoPanel({ ensureSession, onLoaded, disabled }: Props) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const folderInput = useRef<HTMLInputElement>(null)
  const zipInput = useRef<HTMLInputElement>(null)

  async function handleFiles(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return
    setBusy(true)
    setError(null)
    try {
      const sid = await ensureSession()
      const uploads = stripTopDir(await Promise.all(Array.from(fileList).map(readFile)))
      const paths = await uploadRepo(sid, uploads, 'upload')
      onLoaded(sid, paths.find((p) => p.endsWith('.py')) ?? paths[0] ?? null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="repo-panel">
      <div className="repo-actions">
        <button className="repo-btn" disabled={disabled || busy} onClick={() => folderInput.current?.click()}>
          Upload folder
        </button>
        <button className="repo-btn" disabled={disabled || busy} onClick={() => zipInput.current?.click()}>
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
          onChange={(e) => handleFiles(e.target.files)}
        />
        <input
          ref={zipInput}
          type="file"
          hidden
          accept=".zip,application/zip"
          onChange={(e) => handleFiles(e.target.files)}
        />
      </div>

      {/* git field: the Option-A seam, disabled (cloning needs network) */}
      <div className="repo-git" title="git clone needs network; not enabled in this build">
        <input disabled placeholder="git URL (coming soon — needs network)" />
      </div>

      {busy && <div className="repo-status">uploading…</div>}
      {error && <div className="repo-error">{error}</div>}
    </div>
  )
}
