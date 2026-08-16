import { useMemo, useState } from 'react'

// A VS Code-style file explorer: a collapsible tree built from the flat path
// list, a "new file" affordance, and per-file delete. Purely presentational —
// App owns the file state and the create/delete/select actions.

interface TreeNode {
  name: string
  path: string // full repo-relative path (files only)
  dir: boolean
  children: TreeNode[]
}

// buildTree groups sorted paths ("src/a.py", "src/b.py", "main.py") into a
// nested folder structure.
function buildTree(paths: string[]): TreeNode[] {
  const root: TreeNode = { name: '', path: '', dir: true, children: [] }
  for (const p of paths) {
    const parts = p.split('/')
    let node = root
    parts.forEach((part, i) => {
      const isFile = i === parts.length - 1
      let child = node.children.find((c) => c.name === part && c.dir !== isFile)
      if (!child) {
        child = {
          name: part,
          path: isFile ? p : parts.slice(0, i + 1).join('/'),
          dir: !isFile,
          children: [],
        }
        node.children.push(child)
      }
      node = child
    })
  }
  // Folders first, then files, alphabetical within each.
  const sortRec = (n: TreeNode) => {
    n.children.sort((a, b) => (a.dir === b.dir ? a.name.localeCompare(b.name) : a.dir ? -1 : 1))
    n.children.forEach(sortRec)
  }
  sortRec(root)
  return root.children
}

interface Props {
  files: string[]
  selected: string | null
  onSelect: (path: string) => void
  onNewFile: (path: string) => void
  onDelete: (path: string) => void
  disabled: boolean
  hasSession: boolean
}

export function FileExplorer({ files, selected, onSelect, onNewFile, onDelete, disabled, hasSession }: Props) {
  const tree = useMemo(() => buildTree(files), [files])
  const [creating, setCreating] = useState(false)
  const [draft, setDraft] = useState('')

  function commit() {
    const name = draft.trim()
    if (name) onNewFile(name)
    setCreating(false)
    setDraft('')
  }

  return (
    <div className="explorer">
      <div className="explorer-head">
        <span>Files</span>
        <button
          className="explorer-new"
          title={hasSession ? 'new file' : 'creates a sandbox, then a file'}
          disabled={disabled}
          onClick={() => setCreating(true)}
        >
          +
        </button>
      </div>

      {creating && (
        <input
          className="explorer-input"
          autoFocus
          placeholder="path e.g. src/main.py"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') commit()
            if (e.key === 'Escape') {
              setCreating(false)
              setDraft('')
            }
          }}
          onBlur={commit}
        />
      )}

      {files.length === 0 && !creating ? (
        <div className="explorer-empty">
          {hasSession ? 'no files — click + to create one' : 'click + to start a file'}
        </div>
      ) : (
        <div className="explorer-tree">
          {tree.map((n) => (
            <TreeItem
              key={n.path || n.name}
              node={n}
              depth={0}
              selected={selected}
              onSelect={onSelect}
              onDelete={onDelete}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function TreeItem({
  node,
  depth,
  selected,
  onSelect,
  onDelete,
}: {
  node: TreeNode
  depth: number
  selected: string | null
  onSelect: (p: string) => void
  onDelete: (p: string) => void
}) {
  const [open, setOpen] = useState(true)
  const pad = { paddingLeft: 6 + depth * 12 }

  if (node.dir) {
    return (
      <>
        <div className="tree-row tree-dir" style={pad} onClick={() => setOpen(!open)}>
          <span className="tree-caret">{open ? '▾' : '▸'}</span>
          <span className="tree-name">{node.name}</span>
        </div>
        {open &&
          node.children.map((c) => (
            <TreeItem
              key={c.path || c.name}
              node={c}
              depth={depth + 1}
              selected={selected}
              onSelect={onSelect}
              onDelete={onDelete}
            />
          ))}
      </>
    )
  }

  return (
    <div
      className={`tree-row tree-file ${selected === node.path ? 'active' : ''}`}
      style={pad}
      onClick={() => onSelect(node.path)}
    >
      <span className="tree-name">{node.name}</span>
      <button
        className="tree-del"
        title="delete file"
        onClick={(e) => {
          e.stopPropagation()
          onDelete(node.path)
        }}
      >
        ✕
      </button>
    </div>
  )
}
