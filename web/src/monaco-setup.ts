// @monaco-editor/react fetches Monaco from a CDN by default. That would make
// the embedded single-binary build depend on the network, and it would load a
// different monaco version than the one in package.json — bypassing the
// pinned (patched) dompurify. So point the loader at the local package.
//
// Import the editor API directly rather than the `monaco-editor` barrel: the
// barrel pulls in every language Monaco supports plus the json/html/css/ts
// language workers (~14 MB of assets). We run Python only, and Python
// highlighting is tokenizer-based with no language service, so the core editor
// plus Python's own register module is all that is needed.
// (Monaco 0.56 moved these from basic-languages/ to languages/definitions/.)
import * as monaco from 'monaco-editor/editor/editor.api'
import 'monaco-editor/languages/definitions/python/register'
import { loader } from '@monaco-editor/react'
import editorWorker from 'monaco-editor/editor/editor.worker?worker'

self.MonacoEnvironment = {
  getWorker: () => new editorWorker(),
}

loader.config({ monaco })
